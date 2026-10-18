package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
)

// A score.json left by an attempt that died before grade_finished must not outlive a later attempt that
// is refused or fails: the last grade_* event of the ledger is authoritative.
func TestAStaleScoreJSONDoesNotSurviveARefusedOrFailedAttempt(t *testing.T) {
	for name, c := range map[string]struct {
		results string
		edit    func(k *tasktest.Task)
	}{
		"refused":    {tasktest.Results(nil), refusals["missing_suite"].edit},
		"no results": {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			g := startGraded(t, gradedNodes, c.results, nil)
			if c.edit != nil {
				c.edit(g.task)
			}
			stale := filepath.Join(g.r.Dir, "score.json")
			if err := os.WriteFile(stale, []byte(`{"reward":1}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if out := g.advance(t); out.Status != run.StatusFailed {
				t.Fatalf("Advance() = %+v, want a failed run", out)
			}
			if _, err := os.Stat(stale); err == nil {
				t.Error("a stale score.json survived an attempt that did not score")
			}
		})
	}
}

// ygg resume opens the run from disk with the hooks: the reopened run must hold a grader and grade.
func TestAReopenedRunHoldsAGraderAndGrades(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	g.grader.Err = os.ErrDeadlineExceeded
	if out := g.advance(t); out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v", out)
	}
	g.grader.Err = nil
	id := g.r.State.ID
	g.r.Close()
	h := run.Hooks{
		NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return g.box, nil },
		NewGrader:  func(*run.SandboxSetup, string) (run.Grader, error) { return g.grader, nil },
	}
	again, err := run.OpenWith(context.Background(), g.r.Project, id, "", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	if again.Grader == nil {
		t.Fatal("a run opened from disk has no grader")
	}
	g.r = again
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("the resumed Advance() = %+v", out)
	}
	if len(g.grader.Specs) != 2 || readScore(t, again).Reward < 1-1e-9 {
		t.Errorf("grader calls %d, score %+v", len(g.grader.Specs), readScore(t, again))
	}
}
