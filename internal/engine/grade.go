package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/score"
	"github.com/ouroboric-dev/odin/internal/task"
)

// Graded is what a grading run gave: the score and where score.json is.
type Graded struct {
	Score gungnir.Score
	Path  string
	Note  string // why the graded /app holds no release, when a leaf ships a release.sh (run.NoReleaseNote); else ""
}

// GradeFailed is a grading run that could not finish: the container did not run or timed out, the
// suite printed no readable scores, or the project could not be exported (a release that fails or breaks
// its bounds is an export that fails). It is not a refusal and not a score. The run stops failed, and a
// resume grades again.
type GradeFailed struct{ Err error }

func (e *GradeFailed) Error() string { return "grading failed: " + e.Err.Error() }
func (e *GradeFailed) Unwrap() error { return e.Err }

// runGrade is the grade node (spec §11), the last node of a graph: it grades the project the run built.
func runGrade(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	began := time.Now()
	g, err := Grade(ctx, r, "")
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt, DurationMS: time.Since(began).Milliseconds()}
	var refusal *task.Refusal
	var failed *GradeFailed
	switch {
	case err == nil:
		ev.Result = fmt.Sprintf("graded %s: reward %.4f (see score.json in the run folder)", g.Score.Task, g.Score.Reward)
		if g.Note != "" {
			ev.Result += "; " + g.Note
		}
		if err := r.Log(ev); err != nil {
			return err
		}
		r.State.Summaries[n.ID] = ev.Result
		return nil
	case errors.As(err, &refusal), errors.As(err, &failed):
		ev.Name, ev.Result = gungnir.EventNodeFailed, err.Error()
		if lerr := r.Log(ev); lerr != nil {
			return lerr
		}
		return &nodeFailure{err}
	}
	return err // the context ended, or the ledger or the disk failed: Odin's own
}

// Grade grades the project of run r against the hidden suite of a task, and writes score.json to the run
// folder (spec §11, change benchmark-split). taskID names the task; "" takes it from the run's brief,
// which must be the instruction.md of a task in grader.tasks_dir. Grading starts only after every
// sandbox of the run has stopped, the one that builds the releases of the export too (run.SandboxReleaser).
// It refuses, before it builds anything, with a *task.Refusal, which the ledger records as grade_refused; a
// failure of the grading run itself is a *GradeFailed.
//
// The container sees three folders and nothing else (v0-T246): /app, a copy of the project's committed
// files in the run folder; /tests, the suite's tests folder in the store, read only; and /logs/verifier,
// an empty folder in the run folder for the suite's own logs, which Odin never opens: the app shares the
// container and can write there. The scores are the report on the one stdout line that carries the key of
// this attempt (YGG_SCORE_KEY, which the suite keeps from the app); the key is in no event and no state.
// The suite's folder is never given to an agent's sandbox.
func Grade(ctx context.Context, r *run.Run, taskID string) (*Graded, error) {
	cfg := r.Setup.Config.Grader
	if cfg == nil || r.Grader == nil {
		return nil, errors.New("grading needs the grader and the sandbox sections in factory.yaml")
	}
	if err := r.DownSandboxes(context.WithoutCancel(ctx)); err != nil {
		return nil, &GradeFailed{err}
	}
	if err := r.NoSandboxesLeft(); err != nil {
		return nil, &GradeFailed{err}
	}
	// A score.json from an earlier attempt that died before grade_finished must not outlive this one: the
	// last grade_* event of the ledger is authoritative, and a refused or failed attempt leaves no score.
	if err := os.Remove(filepath.Join(r.Dir, "score.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if taskID == "" {
		var ok bool
		if taskID, ok = taskOfBrief(cfg.TasksDir, r.State.Brief); !ok {
			return nil, refused(r, taskID, "", gungnir.GradeBadTask,
				"the run's brief %q is not the instruction.md of a task in %s, so the task to grade is not known (ygg grade --task names it)", r.State.Brief, cfg.TasksDir)
		}
	}
	t, err := task.Load(cfg.TasksDir, cfg.SuiteDir, taskID)
	var refusal *task.Refusal
	if errors.As(err, &refusal) {
		return nil, refused(r, taskID, "", refusal.Reason, "%s", refusal.Detail)
	} else if err != nil {
		return nil, err
	}
	if r.State.BriefSHA256 != t.BriefSHA256 {
		return nil, refused(r, taskID, t.Suite, gungnir.GradeBriefChanged,
			"the run was built from a brief that hashes to %q; the task's brief hashes to %s", r.State.BriefSHA256, t.BriefSHA256)
	}
	image, err := sandbox.GraderImage(t.ID, t.Environment)
	if err != nil {
		return nil, refused(r, taskID, t.Suite, gungnir.GradeBadTask, "%v", err)
	}
	if r.Releaser == nil { // a test's stand-in stays; otherwise the releases are built in the acceptance sandbox
		if r.Releaser, err = r.SandboxReleaser(); err != nil {
			return nil, &GradeFailed{err}
		}
	}
	note, err := r.NoReleaseNote(ctx) // a leaf ships a release.sh, and nothing can build it
	if err != nil {
		return nil, &GradeFailed{err}
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGradeStarted, Task: t.ID, Suite: t.Suite, BriefSHA256: t.BriefSHA256, Image: image, Note: note}); err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	app, logs := filepath.Join(r.GradeDir(), "app"), filepath.Join(r.GradeDir(), "logs-"+started.Format("20060102-150405.000000"))
	if err := r.ExportApp(ctx, app); err != nil {
		return nil, &GradeFailed{err}
	}
	// The release's sandbox stopped inside the export; the grader never starts while it is up.
	if err := r.NoSandboxesLeft(); err != nil {
		return nil, &GradeFailed{err}
	}
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return nil, err
	}
	key, err := scoreKey()
	if err != nil {
		return nil, err
	}
	memory, cpus := cfg.Limits() // "none" and 0 are the operator's choice of no limit, for an engine that cannot apply one
	res, err := r.Grader.Grade(ctx, sandbox.GradeSpec{Run: r.State.ID, Task: t.ID, Environment: t.Environment, App: app, Tests: t.Tests, Logs: logs, ScoreKey: key,
		Timeout: min(time.Duration(cfg.Timeout), t.Timeout), Memory: memory, CPUs: cpus, Env: t.Env})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// The container's output is for the operator: it goes to the run folder, never to an agent.
	if werr := os.WriteFile(filepath.Join(r.GradeDir(), "grade.log"), []byte(res.Log), 0o600); werr != nil {
		return nil, werr
	}
	if err != nil {
		return nil, &GradeFailed{err}
	}
	results, err := score.ParseStdout(res.Stdout, key)
	var mismatch *score.MismatchError
	if errors.As(err, &mismatch) {
		return nil, refused(r, t.ID, t.Suite, gungnir.GradeBadManifest, "%v", err)
	} else if err != nil {
		return nil, &GradeFailed{err}
	}
	s, err := score.Compute(score.Input{Run: r.State.ID, Task: t.ID, TaskVersion: t.Version, Suite: t.Suite, BriefSHA256: t.BriefSHA256,
		Manifest: t.Manifest, Weights: t.Weights, Started: started, Finished: time.Now().UTC()}, results)
	if errors.As(err, &mismatch) {
		return nil, refused(r, t.ID, t.Suite, gungnir.GradeBadManifest, "%v", err)
	} else if err != nil {
		return nil, &GradeFailed{err}
	}
	path, err := score.Write(r.Dir, s)
	if err != nil {
		return nil, err
	}
	reward := s.Reward
	if err := r.Log(gungnir.Event{Name: gungnir.EventGradeFinished, Task: t.ID, Suite: t.Suite, ScorePath: path, Reward: &reward,
		FailToPass: fmt.Sprintf("%d/%d", s.FailToPass.Passed, s.FailToPass.Total), PassToPass: fmt.Sprintf("%d/%d", s.PassToPass.Passed, s.PassToPass.Total)}); err != nil {
		return nil, err
	}
	return &Graded{Score: s, Path: path, Note: note}, r.Save()
}

// scoreKey is the key of one grading attempt: 16 random bytes as 32 lowercase hex characters.
func scoreKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("making the score key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// refused records grade_refused in the ledger and returns the refusal. A failure to write the ledger is
// what comes back instead: Odin's own.
func refused(r *run.Run, taskID, suite string, reason gungnir.GradeReason, format string, a ...any) error {
	ref := &task.Refusal{Reason: reason, Detail: fmt.Sprintf(format, a...)}
	if !task.ValidID(taskID) {
		taskID = "" // the event's schema takes no other id; the refusal's detail still names it
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGradeRefused, Task: taskID, Suite: suite, Reason: reason}); err != nil {
		return err
	}
	return ref
}

// taskOfBrief returns the task id when brief is <tasksDir>/<id>/instruction.md.
func taskOfBrief(tasksDir, brief string) (string, bool) {
	if brief == "" {
		return "", false
	}
	rel, err := filepath.Rel(tasksDir, brief)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if err != nil || len(parts) != 2 || parts[1] != "instruction.md" {
		return "", false
	}
	return parts[0], true
}
