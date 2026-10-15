// Package engine walks a run's graph (spec §4): it runs exec and agent nodes, and stops or passes
// at gate nodes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/run"
)

// ExecTimeout is an exec node's limit when the graph sets none.
const ExecTimeout = 10 * time.Minute

// Outcome is where Advance left the run.
type Outcome struct {
	Status run.Status // done, stopped or failed
	Gate   string     // stopped: the gate node waiting for a decision
	Err    error      // failed: why
}

// Advance runs nodes from the run's position until the graph ends, an approval point stops the
// run, or a node fails. It saves run.json after every node. Every node is safe to re-run: resume
// starts the interrupted node again from scratch (spec §4.3). The returned error is for failures
// of Odin itself (disk, ledger); a failing node is an Outcome.
func Advance(ctx context.Context, r *run.Run) (Outcome, error) {
	s := r.State
	if s.Pending != "" {
		return Outcome{Status: run.StatusStopped, Gate: s.Pending}, nil
	}
	s.Status, s.Error = run.StatusRunning, ""
	if err := r.Save(); err != nil {
		return Outcome{}, err
	}
	nodes := r.Setup.Graph.Nodes
	for s.Position < len(nodes) {
		n := nodes[s.Position]
		var stop bool
		var err error
		switch n.Kind {
		case schema.KindAgent:
			err = runAgent(ctx, r, n)
		case schema.KindExec:
			err = runExec(ctx, r, n)
		case schema.KindGate:
			stop, err = reachGate(r, n)
		case schema.KindMerge:
			err = runMerge(ctx, r, n)
		case schema.KindPointer:
			err = runPointer(ctx, r, n)
		}
		var nf *nodeFailure
		if errors.As(err, &nf) {
			return fail(r, n, nf.err)
		}
		if err != nil {
			return Outcome{}, err
		}
		if stop {
			return Outcome{Status: run.StatusStopped, Gate: n.ID}, nil
		}
		s.Position++
		if err := r.Save(); err != nil {
			return Outcome{}, err
		}
	}
	s.Status = run.StatusDone
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"}.WithUsage(s.Usage)); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: run.StatusDone}, r.Save()
}

// nodeFailure is a node that failed; the run stops failed, and resume re-runs the node.
type nodeFailure struct{ err error }

func (f *nodeFailure) Error() string { return f.err.Error() }

func fail(r *run.Run, n schema.Node, cause error) (Outcome, error) {
	r.State.Status, r.State.Error = run.StatusFailed, fmt.Sprintf("node %s: %v", n.ID, cause)
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunFailed, Node: n.ID, Result: r.State.Error}); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: run.StatusFailed, Err: errors.New(r.State.Error)}, r.Save()
}

// startAttempt counts a new attempt of n and saves it before the node runs, so an attempt that
// is interrupted still counts.
func startAttempt(r *run.Run, n schema.Node) (int, error) {
	r.State.Attempts[n.ID]++
	return r.State.Attempts[n.ID], r.Save()
}

func runAgent(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	role := r.Setup.Config.Roles[n.Role]
	prompt, err := os.ReadFile(filepath.Join(r.Setup.RealmDir, n.Prompt))
	if err != nil {
		return err
	}
	text := string(prompt)
	if r.Setup.BriefText != "" {
		text += "\n\n## Brief\n\n" + r.Setup.BriefText
	}
	if reason := s.Reasons[n.ID]; reason != "" {
		text += "\n\n## Rejected at the approval point\n\n" + reason + "\n"
	}
	ev := gungnir.Event{Node: n.ID, Attempt: attempt, Role: n.Role, Adapter: role.Agent, Model: role.Model}
	start := ev
	start.Name = gungnir.EventNodeStarted
	if err := r.Log(start); err != nil {
		return err
	}
	task := gungnir.Task{Role: n.Role, Node: n.ID, Attempt: attempt, Prompt: text, Workdir: r.RepoDir(n.Repo),
		Model: role.Model, ReadOnly: n.ReadOnly, MaxTurns: n.MaxTurns, Timeout: time.Duration(n.Timeout),
		TranscriptDir: filepath.Join(r.Dir, "transcripts")}
	actx, cancel := ctx, context.CancelFunc(func() {})
	if task.Timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, task.Timeout)
	}
	began := time.Now()
	res, runErr := r.Setup.Agents[n.Role].Run(actx, task)
	cancel()
	if runErr == nil {
		if err := res.Validate(); err != nil {
			runErr = fmt.Errorf("the agent's result is malformed: %w", err)
		} else {
			// Only a well-formed result's usage counts; a malformed one is never trusted.
			s.Usage = s.Usage.Add(res.Usage)
			ev = ev.WithUsage(res.Usage)
			if res.Status != gungnir.StatusOK {
				runErr = fmt.Errorf("the agent reported %s: %s", res.Status, res.Summary)
			}
		}
	}
	ev.DurationMS = time.Since(began).Milliseconds()
	if runErr != nil {
		ev.Name, ev.Result = gungnir.EventNodeFailed, runErr.Error()
		if err := r.Log(ev); err != nil {
			return err
		}
		return &nodeFailure{runErr}
	}
	if ev.Commit, err = checkpoint(ctx, r, n, attempt); err != nil {
		return failCheckpoint(r, ev, err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return err
	}
	delete(s.Reasons, n.ID)
	s.Summaries[n.ID] = res.Summary
	return nil
}

// runExec runs n's command in its repo (see command), and checkpoints the leaf it works in.
func runExec(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	logPath := filepath.Join(r.Dir, "logs", fmt.Sprintf("%s-%d.log", n.ID, attempt))
	began := time.Now()
	runErr, err := command(ctx, r, n.ID, n.Run, r.RepoDir(n.Repo), time.Duration(n.Timeout), logPath)
	if err != nil {
		return err
	}
	result := "exit 0"
	if runErr != nil {
		result = runErr.Error()
	}
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt, Result: result,
		DurationMS: time.Since(began).Milliseconds()}
	if runErr != nil {
		ev.Name = gungnir.EventNodeFailed
	} else if ev.Commit, err = checkpoint(ctx, r, n, attempt); err != nil {
		return failCheckpoint(r, ev, err)
	}
	if err := r.Log(ev); err != nil {
		return err
	}
	if runErr != nil {
		return &nodeFailure{fmt.Errorf("%s: %s (output: %s)", strings.Join(n.Run, " "), result, logPath)}
	}
	r.State.Summaries[n.ID] = result
	return nil
}

// command runs args in dir, without a shell, for node. The command gets only PATH, HOME (the run's
// own directory, so caches never land in a repo) and LANG, so no credential in the operator's
// environment reaches it. Its output goes to logPath. A cancel or timeout (ExecTimeout when 0)
// kills its whole process group (spec §6.2), and run.json records the group while it runs, so a
// later ygg can stop it if this one dies. cmdErr is the command's failure; err is Odin's. Phase 3
// moves leaf commands into the sandbox.
func command(ctx context.Context, r *run.Run, node string, args []string, dir string, timeout time.Duration, logPath string) (cmdErr, err error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if timeout == 0 {
		timeout = ExecTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.Home(), "LANG=C.UTF-8"}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err, nil
	}
	pid := cmd.Process.Pid
	r.State.Exec = &run.ExecProcess{Node: node, PGID: pid, Start: run.ProcStart(pid)}
	if err := r.Save(); err != nil {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
		return nil, err
	}
	cmdErr = cmd.Wait()
	r.State.Exec = nil
	return cmdErr, nil
}

// runChecks runs the pack checks names in dir, in order, for the given attempt of node n, and logs
// each result. The first failing check is the error.
func runChecks(ctx context.Context, r *run.Run, n schema.Node, attempt int, names []string, dir string) error {
	for _, c := range names {
		logPath := filepath.Join(r.Dir, "logs", fmt.Sprintf("%s-%d-%s.log", n.ID, attempt, c))
		cmdErr, err := command(ctx, r, n.ID, r.Setup.Pack.Checks[c], dir, time.Duration(n.Timeout), logPath)
		if err != nil {
			return err
		}
		result := c + ": exit 0"
		if cmdErr != nil {
			result = c + ": " + cmdErr.Error()
		}
		if err := r.Log(gungnir.Event{Name: gungnir.EventCheckResult, Node: n.ID, Attempt: attempt, Result: result}); err != nil {
			return err
		}
		if cmdErr != nil {
			return fmt.Errorf("check %s failed: %v (output: %s)", c, cmdErr, logPath)
		}
	}
	return nil
}

// checkpoint commits node n's work in its leaf to the run's branch, and records the commit in
// run.json: resume resets the leaf to it (spec §4.3). A node that works in an umbrella has no
// checkpoint; the commit that records an umbrella's change is Odin's own, at a pointer move.
func checkpoint(ctx context.Context, r *run.Run, n schema.Node, attempt int) (string, error) {
	rec, ok := r.State.Leaves[n.Repo]
	if !ok {
		return "", nil
	}
	l, err := r.Leaf(n.Repo)
	if err != nil {
		return "", err
	}
	head, _, err := l.Checkpoint(ctx, fmt.Sprintf("ygg: run %s, node %s, attempt %d", r.State.ID, n.ID, attempt))
	if err != nil {
		return "", err
	}
	rec.Checkpoint = head
	r.State.Leaves[n.Repo] = rec
	return head, nil
}

// failCheckpoint logs a node whose work Odin refused to commit, such as a leaf with a planted git
// repository, and fails it. A failure of Odin itself stays an error.
func failCheckpoint(r *run.Run, ev gungnir.Event, err error) error {
	if !leaf.IsPolicy(err) {
		return err
	}
	ev.Name, ev.Result, ev.Commit = gungnir.EventNodeFailed, err.Error(), ""
	if lerr := r.Log(ev); lerr != nil {
		return lerr
	}
	return &nodeFailure{err}
}
