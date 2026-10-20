package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
)

func TestAResultsFileThatDoesNotMatchTheManifestIsRefusedAsBadManifest(t *testing.T) {
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

func TestASuiteThatWritesNoResultsIsAFailureNotAPass(t *testing.T) {
	g := startGraded(t, gradedNodes, "", nil)
	if out := g.advance(t); out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "wrote no results") {
		t.Errorf("Advance() = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
		t.Error("a suite with no results has a score")
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
