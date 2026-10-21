package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
)

func TestAReportThatDoesNotMatchTheManifestIsRefusedAsBadManifest(t *testing.T) {
	missing := strings.Replace(tasktest.Results(nil), `{"title":"a thing is private","tests":[{"results":[{"status":"passed"}]}]}`, ``, 1)
	missing = strings.Replace(missing, `},]`, `}]`, 1)
	extra := strings.TrimSuffix(tasktest.Results(nil), `]}]}`) + `,{"title":"a new thing","tests":[{"results":[{"status":"passed"}]}]}]}]}`
	for name, results := range map[string]string{"a scenario the suite did not run": missing, "a spec the manifest does not know": extra} {
		t.Run(name, func(t *testing.T) {
			g := startGraded(t, gradedNodes, results, nil)
			out := g.advance(t)
			if out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "bad_manifest") {
				t.Fatalf("Advance() = %+v", out)
			}
			if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
				t.Error("a mismatch left a score.json: a scenario on one side only must never score")
			}
			text := ledgerText(t, g.r)
			if !strings.Contains(text, `"event":"grade_started"`) || !strings.Contains(text, `"ygg.grade.reason":"bad_manifest"`) || strings.Contains(text, `"event":"grade_finished"`) {
				t.Errorf("the ledger:\n%s", text)
			}
		})
	}
}

func TestANodeOfAKindTheEngineDoesNotKnowStopsTheRunAsBadManifest(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	nodes := g.r.Setup.Graph.Nodes
	nodes[len(nodes)-1].Kind = "teleport" // the graph loader rejects this kind, so the test sets it after the load
	out := g.advance(t)
	if out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "bad_manifest") || !strings.Contains(out.Err.Error(), "teleport") {
		t.Fatalf("Advance() = %+v, want a failed run whose error says bad_manifest", out)
	}
	if id := nodes[len(nodes)-1].ID; strings.Count(out.Err.Error(), id) != 1 {
		t.Errorf("the error %q names the node %q %d times; the run's error already names it once", out.Err, id, strings.Count(out.Err.Error(), id))
	}
	if len(g.grader.Specs) != 0 {
		t.Errorf("the grader ran %d time(s) for a node of an unknown kind", len(g.grader.Specs))
	}
}

func TestAGradingRunThatFailsFailsTheNodeAndAResumeGradesAgain(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	g.grader.Err = errors.New("the grader timed out after 5m0s")
	out := g.advance(t)
	if out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "timed out") {
		t.Fatalf("Advance() = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
		t.Error("a grading run that failed has a score")
	}
	g.grader.Err = nil
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("the second Advance() = %+v", out)
	}
	if len(g.grader.Specs) != 2 || readScore(t, g.r).Reward < 1-1e-9 {
		t.Errorf("grader calls %d, score %+v", len(g.grader.Specs), readScore(t, g.r))
	}
}

// Every way a stdout can fail to hold exactly one keyed report is a failed grade: no score, a ledger that
// ends at grade_started, and a resume that grades again. It is never a score of 0 and never a refusal.
func TestAStdoutWithoutExactlyOneKeyedReportIsAFailureNotAPass(t *testing.T) {
	const other = "ffffffffffffffffffffffffffffffff"
	report := tasktest.Results(nil)
	for name, c := range map[string]struct {
		stdout func(key string) string
		say    string
	}{
		"no output":           {func(string) string { return "" }, "printed no scores"},
		"no keyed line":       {func(string) string { return "tests ran\nygg-results/v1\n" }, "printed no scores"},
		"another key only":    {func(string) string { return keyedLine(other, report) }, "printed no scores"},
		"two identical lines": {func(k string) string { return keyedLine(k, report) + keyedLine(k, report) }, "printed the scores twice"},
		"two different lines": {func(k string) string {
			return keyedLine(k, report) + keyedLine(k, tasktest.Results(map[string]string{"a thing is private": "failed"}))
		}, "printed the scores twice"},
		"a line that is no report": {func(k string) string { return keyedLine(k, "not json") }, "not a Playwright report"},
		"a report with no spec":    {func(k string) string { return keyedLine(k, `{"suites":[]}`) }, "holds no spec"},
	} {
		t.Run(name, func(t *testing.T) {
			g := startGraded(t, gradedNodes, "", nil)
			g.grader.Stdout = c.stdout
			out := g.advance(t)
			// The node's failure carries the text of the error, not the error: *GradeFailed is told by its prefix.
			if out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "grading failed: ") || !strings.Contains(out.Err.Error(), c.say) {
				t.Fatalf("Advance() = %+v, want a failed run, a grading failure that says %q", out, c.say)
			}
			if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
				t.Error("a stdout with no usable report left a score.json")
			}
			text := ledgerText(t, g.r)
			if !strings.Contains(text, `"event":"grade_started"`) || strings.Contains(text, `"event":"grade_finished"`) || strings.Contains(text, `"event":"grade_refused"`) {
				t.Errorf("the ledger:\n%s", text)
			}
			var failed *GradeFailed
			if _, err := Grade(context.Background(), g.r, ""); !errors.As(err, &failed) {
				t.Errorf("Grade() = %T %v, want a *GradeFailed", err, err)
			}
			g.grader.Stdout = nil
			g.grader.Results = report
			if out := g.advance(t); out.Status != run.StatusDone || len(g.grader.Specs) != 3 {
				t.Fatalf("the resumed Advance() = %+v after %d grading run(s)", out, len(g.grader.Specs))
			}
		})
	}
}

// The suite shares its container with the app, and a results.json in the logs folder is the app's to forge.
// Odin does not open it.
func TestAResultsFileInTheLogsFolderChangesNothing(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(map[string]string{"a thing is private": "failed"}), nil)
	g.grader.Forged = tasktest.Results(nil) // every scenario passes
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	if s := readScore(t, g.r); s.Reward > 0.6+1e-9 || s.FailToPass != (gungnir.Tally{Passed: 2, Total: 3}) {
		t.Errorf("the score followed the forged file: %+v", s)
	}
	// With no keyed line, a forged file is no score at all.
	g = startGraded(t, gradedNodes, "", nil)
	g.grader.Forged = tasktest.Results(nil)
	if out := g.advance(t); out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "printed no scores") {
		t.Errorf("Advance() = %+v", out)
	}
}

// A line that carries another key is the app's, and it does not stand next to the suite's line.
func TestALineOfAnotherKeyIsIgnoredBesideTheSuitesLine(t *testing.T) {
	g := startGraded(t, gradedNodes, "", nil)
	g.grader.Stdout = func(k string) string {
		return keyedLine("ffffffffffffffffffffffffffffffff", tasktest.Results(nil)) + keyedLine(k, tasktest.Results(map[string]string{"a thing is private": "failed"}))
	}
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	if s := readScore(t, g.r); s.FailToPass != (gungnir.Tally{Passed: 2, Total: 3}) {
		t.Errorf("score = %+v", s)
	}
}

// The key is Odin's and the container's for one attempt: 32 hex characters, new each time, in no file the
// run keeps but grade.log (the tail of the container's own output, which the suite may have echoed it into).
func TestEachGradingAttemptHasItsOwnKeyAndTheKeyIsNotInTheLedgerOrTheState(t *testing.T) {
	g := startGraded(t, gradedNodes, "", nil)
	g.grader.Stdout = func(string) string { return "" }
	g.advance(t)
	g.grader.Stdout = nil
	g.grader.Results = tasktest.Results(nil)
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	if len(g.grader.Specs) != 2 {
		t.Fatalf("%d grading runs, want 2", len(g.grader.Specs))
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	k1, k2 := g.grader.Specs[0].ScoreKey, g.grader.Specs[1].ScoreKey
	if !hex32.MatchString(k1) || !hex32.MatchString(k2) || k1 == k2 {
		t.Fatalf("keys %q and %q: want two different 32-character hex strings", k1, k2)
	}
	state, _ := os.ReadFile(filepath.Join(g.r.Dir, "run.json"))
	score, _ := os.ReadFile(filepath.Join(g.r.Dir, "score.json"))
	text := ledgerText(t, g.r)
	for _, k := range []string{k1, k2} {
		if strings.Contains(text, k) || strings.Contains(string(state), k) || strings.Contains(string(score), k) {
			t.Errorf("the key %s is in the ledger, the state or score.json", k)
		}
	}
}

func TestARunBuiltFromAnotherBriefThanTheTasksIsRefused(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	// The task moves to a new version after the run began: the file and its hash agree, but not with the run's.
	brief := tasktest.Brief + "A newer brief.\n"
	sum := sha256.Sum256([]byte(brief))
	toml, _ := os.ReadFile(filepath.Join(g.task.Dir, "task.toml"))
	old := sha256.Sum256([]byte(tasktest.Brief))
	os.WriteFile(filepath.Join(g.task.Dir, "task.toml"), []byte(strings.Replace(string(toml), hex.EncodeToString(old[:]), hex.EncodeToString(sum[:]), 1)), 0o644)
	os.WriteFile(g.task.Brief(), []byte(brief), 0o644)
	out := g.advance(t)
	if out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "brief_changed") || len(g.grader.Specs) != 0 {
		t.Errorf("Advance() = %+v, grader calls %d", out, len(g.grader.Specs))
	}
}
