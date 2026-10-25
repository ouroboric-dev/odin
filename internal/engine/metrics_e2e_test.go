package engine

import (
	"context"
	"testing"

	"github.com/ouroboric-dev/odin/internal/metrics"
)

// Attempts 1 and 2 fail the unit check, so the reviewer first runs in loop attempt 3 (its own session 1), raises a
// major, and the loop gives up. The true veto rate is 1 of 1 reviewed attempts: the metrics must read the ledger
// the engine writes, where a reviewer's counter and the loop's attempt drift apart.
func TestMetricsReadTheVetoRateOfARealLoopWhereTheReviewerFirstRunsInAttempt3(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "todo again") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [ " + majorFinding + " ] }\n"
	r := startReview(t, "key-points", script)
	if _, err := Advance(context.Background(), r); err != nil {
		t.Logf("Advance: %v", err)
	}
	m, err := metrics.Collect(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Review.Vetoes.N != 1 || m.Review.Vetoes.D != 1 {
		t.Errorf("veto rate %d of %d, want 1 of 1", m.Review.Vetoes.N, m.Review.Vetoes.D)
	}
	if m.Findings.RaisedByReview != 1 || m.Findings.RaisedByCheck < 1 || m.Findings.RaisedUnattributed != 0 {
		t.Errorf("Findings = %+v; want the reviewer's major attributed to review and the failed checks to a check", m.Findings)
	}
	if m.Loops.Finished != 1 || m.Loops.GaveUp != 1 {
		t.Errorf("Loops = %+v; want one loop, given up", m.Loops)
	}
	if len(m.Interventions.Items) != 1 || m.Interventions.Items[0].Note == "" || m.Interventions.Items[0].Note == "stopped" {
		t.Errorf("Interventions = %+v; want one escalation whose note is its reason", m.Interventions)
	}
}

func usedStep(node, file string) string {
	return "  " + node + ":\n    - actions: [ { write: { path: " + file + ", content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote " + file + ", usage: { in: 100, out: 10, turns: 3, cost_usd: 0.5 }, output: '{\"summary\": \"done\"}' }\n"
}

// A session line carries no task id; the metrics attribute it to its task by the node's slug prefix.
func TestMetricsAttributeTheSessionsOfARealRunToTheirTasks(t *testing.T) {
	r := galleryRun(t, "unattended", usedStep("p-t001-implement", "api.txt")+usedStep("p-t002-implement", "web.txt"))
	if out, err := Advance(context.Background(), r); err != nil {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	m, err := metrics.Collect(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tasks) != 2 || m.Usage.Turns != 6 {
		t.Fatalf("Tasks = %d, run turns = %d; want two tasks and 6 turns", len(m.Tasks), m.Usage.Turns)
	}
	sum := 0
	for _, tk := range m.Tasks {
		if tk.Usage.Turns != 3 {
			t.Errorf("task %s has %d turns, want 3", tk.ID, tk.Usage.Turns)
		}
		sum += int(tk.Usage.Turns)
	}
	if sum != int(m.Usage.Turns) {
		t.Errorf("the tasks add up to %d turns, the run to %d", sum, m.Usage.Turns)
	}
	if m.Loops.Passed != 2 || m.Loops.Finished != 2 {
		t.Errorf("Loops = %+v; want two loops, both passed", m.Loops)
	}
	for leaf, g := range m.Leaves {
		if g.Usage.Turns != 3 {
			t.Errorf("leaf %s has %d turns, want 3", leaf, g.Usage.Turns)
		}
	}
}
