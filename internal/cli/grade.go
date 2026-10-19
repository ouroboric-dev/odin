package cli

import (
	"errors"
	"flag"
	"fmt"

	"github.com/ouroboric-dev/odin/internal/engine"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task"
)

// grade runs ygg grade: it grades the project of a finished run against the hidden suite of a task and
// writes score.json to the run's folder (spec §11). A run whose graph has a grade node is graded at its
// end; this command grades a run that has none, or grades again after a refusal was fixed. It prints the
// score to the operator, and to nobody else. A refusal exits 2 and builds nothing.
func (c *command) grade(args []string) int {
	f, pos, ok := c.parse("grade", args, 1, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.task, "task", "", "the task to grade against: a folder of grader.tasks_dir; the default is the task whose instruction.md was the run's brief")
	})
	if !ok {
		return ExitRefused
	}
	r, code := c.open(f, pos[0])
	if r == nil {
		return code
	}
	defer r.Close()
	switch {
	case r.State.Status != run.StatusDone:
		return c.refuse(fmt.Errorf("run %s is %s; grade a run that is done, because its sandboxes and its work are final only then", r.State.ID, r.State.Status))
	case r.Setup.Config.Grader == nil || r.Grader == nil:
		return c.refuse(fmt.Errorf("%s needs the grader and the sandbox sections to grade a run", r.Setup.ConfigPath))
	}
	g, err := engine.Grade(c.ctx, r, f.task)
	var refusal *task.Refusal
	switch {
	case errors.As(err, &refusal):
		return c.refuse(err)
	case err != nil:
		fmt.Fprintf(c.err, "ygg: run %s: %v\n", r.State.ID, err)
		return ExitFailed
	}
	s := g.Score
	fmt.Fprintf(c.out, "Graded run %s: %s %s against suite %s.\nreward:        %.4f\nfail-to-pass:  %d/%d\npass-to-pass:  %d/%d\nscore:         %s\n",
		r.State.ID, s.Task, s.TaskVersion, s.Suite, s.Reward, s.FailToPass.Passed, s.FailToPass.Total, s.PassToPass.Passed, s.PassToPass.Total, g.Path)
	return ExitDone
}
