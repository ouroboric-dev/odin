package engine

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
)

// The retrospective (change typed-boundaries §5): an agent after the grade, in a scratch workspace, that reads the
// run record and returns the retro.
const retroNode = "  - { id: retro, kind: agent, role: retrospective, prompt: p.md, scratch: true, input: [record], output: ygg.retro/v1 }\n"

const retroRoles = "roles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n  retrospective: { agent: scripted, model: none, script: script.yaml }\n"

const retroOutput = `{"went_well": [{"category": "navigation", "severity": "low", "text": "the design came first time", "block": "design", "evidence": ["ledger:3"]}], "went_wrong": [], "suggestions": []}`

const retroScript = gradedScript + "  retro:\n    - result: { status: ok, summary: looked back, output: '" + retroOutput + "' }\n"

// partialResults is a report of the stand-in suite that holds one of its four scenarios: the grade refuses it, and
// its refusal names the three it misses.
const partialResults = `{"suites":[{"title":"stand-in.spec.ts","specs":[{"title":"a thing works","tests":[{"results":[{"status":"passed"}]}]}]}]}`

// hiddenWords are what the stand-in suite holds and the retrospective must never see: its scenario ids and its spec file.
var hiddenWords = append([]string{"stand-in.spec.ts"}, tasktest.Titles...)

func startRetro(t *testing.T, script, results string) *graded {
	t.Helper()
	g, err := startGradedWith(t, gradedRun{nodes: gradedNodes + retroNode, results: results, script: script, roles: retroRoles})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// recordSection is the record input of the retro's first session: in its prompt, or in the file the prompt names.
func recordSection(t *testing.T, r *run.Run) string {
	t.Helper()
	prompt := promptOf(t, r, "retro-1")
	_, body, ok := strings.Cut(prompt, "## Input: record\n\n")
	if !ok {
		t.Fatalf("the retro's prompt has no record input:\n%s", prompt)
	}
	if rel, ok := strings.CutPrefix(body, "Your input record is in "); ok {
		rel, _, _ = strings.Cut(rel, ":")
		b, err := os.ReadFile(filepath.Join(r.ScratchDir("retro"), rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body, _, _ = strings.Cut(body, "\n## ")
	return body
}

// jsonField is the JSON block under "### name" in a rendered input.
func jsonField(t *testing.T, section, name string) map[string]any {
	t.Helper()
	_, rest, ok := strings.Cut(section, "### "+name+"\n\n```json\n")
	if !ok {
		t.Fatalf("the record input has no %s block:\n%s", name, section)
	}
	block, _, _ := strings.Cut(rest, "```\n")
	var v map[string]any
	if err := json.Unmarshal([]byte(block), &v); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, block)
	}
	return v
}

func readRecordJSON(t *testing.T, r *run.Run) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, metrics.RecordFile))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheRetroRunsAfterTheGradeAndSeesTotalsOnly(t *testing.T) {
	g := startRetro(t, retroScript, partialResults)
	// The first grade is refused: the suite's report misses scenarios, and the refusal names them in the ledger.
	if out := g.advance(t); out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, want the grade to fail", out)
	}
	if _, err := os.Stat(filepath.Join(g.r.Dir, "transcripts", "retro-1.json")); err == nil {
		t.Fatal("the retro ran though the grade failed")
	}
	if _, err := os.Stat(filepath.Join(g.r.Dir, metrics.RecordFile)); err != nil {
		t.Errorf("a failed run has no record.json: %v", err)
	}
	if text := ledgerText(t, g.r); !strings.Contains(text, "a thing is private") {
		t.Fatalf("the refused grade's lines do not name the missing scenario, so this test proves nothing:\n%s", text)
	}
	// The operator judges the run; the retrospective must not see it.
	judged := schema.Judgement{Run: g.r.State.ID, Score: 7, Well: "the operator liked the quiet design", Wrong: "the operator missed a check", Time: "2026-10-25T10:00:00Z"}
	if err := metrics.WriteJudgements(g.r.Dir, []schema.Judgement{judged}); err != nil {
		t.Fatal(err)
	}
	// ygg score logs the score as a judged line too (cli/score.go).
	if err := g.r.Log(gungnir.Event{Name: gungnir.EventJudged, Result: strconv.Itoa(judged.Score), Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	g.grader.Results = tasktest.Results(map[string]string{"a thing is private": "failed"})
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}

	// The retro runs last, after the grade has finished.
	text := ledgerText(t, g.r)
	graded := strings.LastIndex(text, `"event":"grade_finished"`)
	started := strings.Index(text, `"ygg.node.id":"retro","event":"node_started"`)
	if graded < 0 || started < graded {
		t.Errorf("the retro did not start after the grade finished (grade_finished at %d, retro's node_started at %d)", graded, started)
	}

	// Its record input holds the grade's totals, and nothing that names a test, a category or a judgement.
	section := recordSection(t, g.r)
	grade := jsonField(t, section, "grade")
	if grade["reward"] != 0.6 || grade["task"] != "tiny-task" {
		t.Errorf("the grade = %v", grade)
	}
	if f, p := grade["fail_to_pass"], grade["pass_to_pass"]; !reflect.DeepEqual(f, map[string]any{"passed": 2.0, "total": 3.0}) || !reflect.DeepEqual(p, map[string]any{"passed": 1.0, "total": 1.0}) {
		t.Errorf("the grade's tallies = %v and %v", f, p)
	}
	if c, ok := grade["categories"].(map[string]any); !ok || len(c) != 0 {
		t.Errorf("the grade's categories = %v, want none: the totals only", grade["categories"])
	}
	for _, field := range []string{"### judgements", "### retro"} {
		if strings.Contains(section, field) {
			t.Errorf("the record input holds %s:\n%s", field, section)
		}
	}

	// Nothing the retro can read names a test of the suite or holds the judgement: its prompt, and every file of its
	// workspace, the record input, the transcripts and the ledger.
	seen := map[string]string{"the prompt": promptOf(t, g.r, "retro-1")}
	ws := g.r.ScratchDir("retro")
	filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(ws, p)
			seen[rel] = string(b)
		}
		return nil
	})
	for name, body := range seen {
		for _, w := range append(hiddenWords, judged.Well) {
			if strings.Contains(body, w) {
				t.Errorf("%s holds %q", name, w)
			}
		}
	}
	// The workspace holds the run's transcripts and its ledger, which keeps the grade's totals.
	if _, ok := seen[filepath.Join("transcripts", "design-1.json")]; !ok {
		t.Errorf("the workspace has no copy of the design's transcript: %v", keysOf(seen))
	}
	if _, ok := seen[filepath.Join("transcripts", "retro-1.json")]; ok {
		t.Error("the workspace holds the retro's own transcript")
	}
	led := seen["ledger.jsonl"]
	if !strings.Contains(led, `"event":"grade_finished"`) || !strings.Contains(led, `"ygg.grade.fail_to_pass":"2/3"`) || !strings.Contains(led, `"event":"grade_refused"`) {
		t.Errorf("the ledger's copy lacks the grade's totals:\n%s", led)
	}
	if !strings.Contains(text, `"event":"judged"`) {
		t.Fatalf("the run's ledger has no judged line, so this test proves nothing:\n%s", text)
	}
	if strings.Contains(led, `"event":"judged"`) {
		t.Errorf("the ledger's copy holds the operator's judgement:\n%s", led)
	}
	if want := strings.Count(text[:started], "\n") - 1; strings.Count(led, "\n") != want {
		t.Errorf("the ledger's copy has %d lines, want the %d written before the retro started, but the judged one", strings.Count(led, "\n"), want)
	}
	prompt := seen["the prompt"]
	for _, want := range []string{"transcripts/", "ledger.jsonl"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not tell the retro of %s:\n%s", want, prompt)
		}
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestTheRetroReachesTheRecord(t *testing.T) {
	g := startRetro(t, retroScript, tasktest.Results(map[string]string{"a thing is private": "failed"}))
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	rec := readRecordJSON(t, g.r)
	var want any
	if err := json.Unmarshal([]byte(retroOutput), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec["retro"], want) {
		t.Errorf("record.json's retro = %v, want %v", rec["retro"], want)
	}
	// The record itself keeps the whole grade: only the retro's view of it is the totals.
	grade, _ := rec["grade"].(map[string]any)
	if c, _ := grade["categories"].(map[string]any); len(c) != 2 || rec["status"] != "done" {
		t.Errorf("record.json: status %v, grade %v", rec["status"], grade)
	}
	m, err := metrics.Collect(g.r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Retro) == 0 {
		t.Error("Collect read no retro")
	}
}

func TestAFailingRetroFailsTheRunAndTheGradeStaysInTheRecord(t *testing.T) {
	bad := "    - result: { status: ok, summary: looked back, output: '{\"went_well\": \"all of it\"}' }\n"
	g := startRetro(t, gradedScript+"  retro:\n"+bad+bad, tasktest.Results(nil))
	out := g.advance(t)
	if out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "node retro") {
		t.Fatalf("Advance() = %+v, want the retro to fail the run", out)
	}
	rec := readRecordJSON(t, g.r)
	grade, _ := rec["grade"].(map[string]any)
	if grade["reward"] != 1.0 || rec["status"] != "failed" {
		t.Errorf("record.json: status %v, grade %v; want the failed run with its grade", rec["status"], grade)
	}
	if _, ok := rec["retro"]; ok {
		t.Errorf("record.json holds a retro the run refused: %v", rec["retro"])
	}
}

func TestAFactoryWithoutTheRetrospectiveRoleIsRefused(t *testing.T) {
	_, err := startGradedWith(t, gradedRun{nodes: gradedNodes + retroNode, results: tasktest.Results(nil), script: retroScript})
	if err == nil || !strings.Contains(err.Error(), "node retro uses role retrospective") || !strings.Contains(err.Error(), "doesn't define") {
		t.Fatalf("run.Start() = %v, want a refusal naming the role retrospective", err)
	}
}

// R54: whichever way a run ends, done, failed or stopped at a gate, its folder holds the record.
func TestEveryWayARunEndsWritesItsRecord(t *testing.T) {
	for name, c := range map[string]struct {
		nodes  string
		status run.Status
	}{
		"done":   {"  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n", run.StatusDone},
		"failed": {"  - { id: x, kind: exec, run: [\"false\"], repo: repos/app }\n", run.StatusFailed},
		"stopped": {"  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n" +
			"  - { id: g, kind: gate, gate: design }\n", run.StatusStopped},
	} {
		t.Run(name, func(t *testing.T) {
			r := start(t, "key-points", "", c.nodes, okScript)
			out, err := Advance(context.Background(), r)
			if err != nil || out.Status != c.status {
				t.Fatalf("Advance() = %+v, %v; want %s", out, err, c.status)
			}
			if len(out.Warn) != 0 {
				t.Errorf("Warn = %v", out.Warn)
			}
			if rec := readRecordJSON(t, r); rec["run"] != r.State.ID {
				t.Errorf("record.json = %v", rec)
			}
		})
	}
}

func TestTheTranscriptsCopyStopsAtTheBound(t *testing.T) {
	if maxRetroTranscripts != 2000 || maxRetroTranscriptBytes != 64<<20 {
		t.Errorf("the bounds are %d files and %d bytes, want 2000 and 64 MiB", maxRetroTranscripts, maxRetroTranscriptBytes)
	}
	src := t.TempDir()
	for name, size := range map[string]int{"a-1.json": 10, "b-1.json": 10, "c-1.json": 30, "d-1.json": 10, "e-1.json": 10} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not a transcript"), 0o644)
	if err := os.Symlink(outside, filepath.Join(src, "link.json")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		files int
		bytes int64
		want  []string
		count transcriptCopy
	}{
		// The count runs out after three files: the other two are left out.
		"files": {3, 1 << 20, []string{"a-1.json", "b-1.json", "c-1.json"}, transcriptCopy{copied: 3, leftOut: 2, total: 5}},
		// c is larger than the 15 bytes left and is skipped; d still fits, and then e does not.
		"bytes": {100, 35, []string{"a-1.json", "b-1.json", "d-1.json"}, transcriptCopy{copied: 3, skipped: 2, total: 5}},
		"all":   {100, 1 << 20, []string{"a-1.json", "b-1.json", "c-1.json", "d-1.json", "e-1.json"}, transcriptCopy{copied: 5, total: 5}},
	} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "transcripts")
			got, err := copyTranscripts(src, dst, c.files, c.bytes)
			if err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(dst)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if got != c.count || !reflect.DeepEqual(names, c.want) {
				t.Errorf("copied %+v: %v; want %+v: %v", got, names, c.count, c.want)
			}
		})
	}
	// A run with no transcripts gives an empty folder.
	dst := filepath.Join(t.TempDir(), "transcripts")
	if got, err := copyTranscripts(filepath.Join(src, "none"), dst, 10, 100); err != nil || got != (transcriptCopy{}) {
		t.Errorf("copyTranscripts(no folder) = %+v, %v", got, err)
	}
	if st, err := os.Stat(dst); err != nil || !st.IsDir() {
		t.Errorf("no transcripts/ folder: %v", err)
	}
	// A transcripts folder that is a link is never followed: the copy is refused, and says why.
	link := filepath.Join(t.TempDir(), "transcripts")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	dst = filepath.Join(t.TempDir(), "transcripts")
	if got, err := copyTranscripts(link, dst, 10, 1<<20); err == nil || !strings.Contains(err.Error(), "is a link, not a folder of the run's own") || got.copied != 0 {
		t.Errorf("copyTranscripts(a link) = %+v, %v; want it refused", got, err)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("a linked folder's files were copied: %v", entries)
	}
	// The prompt says when the copy is a part, and how much of it was skipped or left out.
	if note := retroNote(transcriptCopy{copied: 3, skipped: 1, leftOut: 1, total: 5}); !strings.Contains(note, "3 of the run's 5 transcript files") ||
		!strings.Contains(note, "1 skipped") || !strings.Contains(note, "1 left out") || !strings.Contains(note, "2000 files and 64 MiB") {
		t.Errorf("retroNote(3 of 5) = %q", note)
	}
	if note := retroNote(transcriptCopy{copied: 5, total: 5}); !strings.Contains(note, "all 5 of the run's transcript files") || strings.Contains(note, "skipped") {
		t.Errorf("retroNote(5 of 5) = %q", note)
	}
}

// A judgements.json that can't be read, or a record.json that can't be written, does not stop the retrospective:
// its view of the record stands without them, and the operator is warned.
func TestTheRetroRunsWhenTheFullRecordCannotBeWritten(t *testing.T) {
	g := startRetro(t, retroScript, tasktest.Results(nil))
	g.grader.Before = func(sandbox.GradeSpec) {
		// The judgement breaks its contract (a score of 11), and record.json is a folder no rename replaces.
		bad := `[{"run": "` + g.r.State.ID + `", "score": 11, "well": "the operator liked it", "wrong": "x", "time": "2026-10-25T10:00:00Z"}]`
		os.WriteFile(filepath.Join(g.r.Dir, metrics.JudgementsFile), []byte(bad), 0o644)
		os.RemoveAll(filepath.Join(g.r.Dir, metrics.RecordFile))
		os.MkdirAll(filepath.Join(g.r.Dir, metrics.RecordFile, "keep"), 0o755)
	}
	out := g.advance(t)
	if out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, want the retro to run", out)
	}
	if _, ok := g.r.State.Outputs["retro"]; !ok {
		t.Error("the retro kept no output")
	}
	warned := strings.Join(out.Warn, "\n")
	if !strings.Contains(warned, "record.json could not be written") || !strings.Contains(warned, "ygg record "+g.r.State.ID) {
		t.Errorf("Warn = %q; want the record's warning with its next step", out.Warn)
	}
	if strings.Contains(promptOf(t, g.r, "retro-1"), "the operator liked it") || strings.Contains(recordSection(t, g.r), "judgements") {
		t.Errorf("the retro saw the judgements:\n%s", recordSection(t, g.r))
	}
}
