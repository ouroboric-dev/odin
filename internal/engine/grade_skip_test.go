package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
)

// A factory with no grader section skips the grade node, and the run goes on to the retrospective (ruling R63).
func TestAGradeNodeWithoutAGraderIsSkippedAndTheRunGoesOn(t *testing.T) {
	const skipped = "graded: skipped, no grader in factory.yaml"
	g, err := startGradedWith(t, gradedRun{nodes: buildNodes + "  - { id: score, kind: grade }\n" + retroNode, results: tasktest.Results(nil),
		script: buildScript + "  retro:\n    - result: { status: ok, summary: looked back, output: '" + retroOutput + "' }\n", roles: retroRoles, noGrader: true})
	if err != nil {
		t.Fatalf("Start() = %v; a grade node needs no grader", err)
	}
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, want the run done", out)
	}
	if len(g.grader.Specs) != 0 {
		t.Errorf("the grader ran %d time(s)", len(g.grader.Specs))
	}

	// The ledger holds the skip as the grade node's result, and no grading.
	text := ledgerText(t, g.r)
	if !strings.Contains(text, `"ygg.node.id":"score","event":"node_finished"`) || !strings.Contains(text, `"ygg.result":"`+skipped+`"`) {
		t.Errorf("the ledger does not hold the skip:\n%s", text)
	}
	for _, ev := range []string{"grade_started", "grade_finished", "grade_refused"} {
		if strings.Contains(text, `"event":"`+ev+`"`) {
			t.Errorf("the ledger holds %s:\n%s", ev, text)
		}
	}
	// The node is done: its summary says so, and a resume does not run it again.
	if s := g.r.State; s.Summaries["score"] != skipped || s.Attempts["score"] != 1 {
		t.Errorf("the grade node: summary %q, attempts %d", s.Summaries["score"], s.Attempts["score"])
	}
	if out := g.advance(t); out.Status != run.StatusDone || g.r.State.Attempts["score"] != 1 || g.r.State.Attempts["retro"] != 1 {
		t.Errorf("a resume: %+v, attempts %v", out, g.r.State.Attempts)
	}

	// The retro ran, on a record with no grade, and its workspace note says the run was not graded.
	if _, err := os.Stat(filepath.Join(g.r.Dir, "transcripts", "retro-1.json")); err != nil {
		t.Fatalf("the retro did not run: %v", err)
	}
	if section := recordSection(t, g.r); strings.Contains(section, "### grade") {
		t.Errorf("the retro's record input holds a grade:\n%s", section)
	}
	if prompt := promptOf(t, g.r, "retro-1"); !strings.Contains(prompt, "The run was not graded") {
		t.Errorf("the retro's prompt does not say the run was not graded:\n%s", prompt)
	}

	// record.json validates, with the retro and no grade.
	b, err := os.ReadFile(filepath.Join(g.r.Dir, metrics.RecordFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(schema.ContractRecord, b); err != nil {
		t.Errorf("record.json does not validate: %v", err)
	}
	if rec := readRecordJSON(t, g.r); rec["grade"] != nil || rec["retro"] == nil {
		t.Errorf("record.json: grade %v, retro %v; want the retro and no grade", rec["grade"], rec["retro"])
	}

	// The metrics, which ygg report renders, say why there is no grade.
	m, err := metrics.Collect(g.r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var w strings.Builder
	m.WriteText(&w)
	if !strings.Contains(w.String(), "grade:        skipped, no grader in factory.yaml") {
		t.Errorf("the metrics do not say the grade was skipped:\n%s", w.String())
	}
}
