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

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gleipnir/scan"
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
	Warn   []string   // what the run couldn't clean up, such as a sandbox that wouldn't stop; doctor finds it
}

// Advance runs nodes from the run's position until the graph ends, an approval point stops the
// run, or a node fails. It saves run.json after every node. Every node is safe to re-run: resume
// starts the interrupted node again from scratch (spec §4.3). The returned error is for failures
// of Odin itself (disk, ledger); a failing node is an Outcome.
func Advance(ctx context.Context, r *run.Run) (out Outcome, err error) {
	// Every sandbox this call started stops when it returns, whatever way it returns. A sandbox that
	// won't stop stays recorded in run.json, and the next resume or ygg doctor stops it.
	defer func() {
		if derr := r.DownSandboxes(context.WithoutCancel(ctx)); derr != nil {
			out.Warn = append(out.Warn, derr.Error())
		}
	}()
	return advance(ctx, r)
}

func advance(ctx context.Context, r *run.Run) (Outcome, error) {
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
		var hs *hardStop
		if errors.As(err, &hs) {
			return stopHard(r, n, hs)
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
	prompt, err := os.ReadFile(filepath.Join(r.Setup.RealmDir, n.Prompt))
	if err != nil {
		return err
	}
	text := string(prompt)
	// Skills load explicitly, the ones this node names, and no others (spec §5.1).
	skillText, err := r.Setup.Libs.Load(n.Skills)
	if err != nil {
		return err
	}
	if skillText != "" {
		text += "\n\n" + skillText
	}
	if r.Setup.BriefText != "" {
		text += "\n\n## Brief\n\n" + r.Setup.BriefText
	}
	if reason := s.Reasons[n.ID]; reason != "" {
		text += "\n\n## Rejected at the approval point\n\n" + reason + "\n"
	}
	res, ev, err := session(ctx, r, sessionSpec{key: n.ID, role: n.Role, repo: n.Repo, text: text, readOnly: n.ReadOnly,
		maxTurns: n.MaxTurns, timeout: time.Duration(n.Timeout)})
	if err != nil {
		return nodeError(err)
	}
	if n.ReadOnly {
		if err := readOnly(ctx, r, n); err != nil {
			return failCheckpoint(r, ev, err)
		}
	}
	if ev.Commit, err = checkpoint(ctx, r, n, ev.Attempt); err != nil {
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
	runErr, err := command(ctx, r, n.ID, n.Repo, r.RepoDir(n.Repo), n.Run, time.Duration(n.Timeout), logPath)
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

// command runs args in repo, without a shell, for node. In a leaf of a run with a sandbox it runs in
// the leaf's sandbox, which holds no credential and no host path but the leaf (spec §6); anywhere
// else, which is Odin's own generators in an umbrella and the whole of a run without a sandbox, it
// runs on the host with only PATH, HOME (the run's own directory, so caches never land in a repo)
// and LANG. Its output goes to logPath. A timeout (ExecTimeout when 0) or a cancel stops it: on the
// host by killing its whole process group, in the sandbox by killing what it started there (spec
// §6.2). cmdErr is the command's failure; err is Odin's, and a sandbox that won't start is a
// *sandboxFailure or a *hardStop.
func command(ctx context.Context, r *run.Run, node, repo, dir string, args []string, timeout time.Duration, logPath string) (cmdErr, err error) {
	if timeout == 0 {
		timeout = ExecTimeout
	}
	runner, err := r.Runner(ctx, repo)
	if err != nil {
		return nil, sandboxStart(err)
	}
	if runner != nil {
		return inSandbox(ctx, runner, args, timeout, logPath)
	}
	return onHost(ctx, r, node, args, dir, timeout, logPath)
}

// inSandbox runs args through runner, with no environment of its own.
func inSandbox(ctx context.Context, runner gungnir.Runner, args []string, timeout time.Duration, logPath string) (cmdErr, err error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := runner.Run(cctx, gungnir.Cmd{Argv: args, Stdout: out, Stderr: out})
	switch {
	case err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("timed out after %s", timeout), nil
	case err != nil && ctx.Err() == nil:
		return nil, err
	case err != nil:
		return nil, ctx.Err()
	case res.ExitCode != 0:
		return fmt.Errorf("exit status %d", res.ExitCode), nil
	}
	return nil, nil
}

// onHost runs args in dir on the host, in its own process group.
func onHost(ctx context.Context, r *run.Run, node string, args []string, dir string, timeout time.Duration, logPath string) (cmdErr, err error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
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

// runChecks runs the pack checks names in order, for the given attempt of node n, and logs each
// result. The first failing check is the error. repo is the leaf they check, whose sandbox runs them
// when the run has one; the pointer node's acceptance checks pass "" and the umbrella's dir, and run
// on the host.
func runChecks(ctx context.Context, r *run.Run, n schema.Node, attempt int, names []string, repo, dir string) error {
	for _, c := range names {
		logPath := filepath.Join(r.Dir, "logs", fmt.Sprintf("%s-%d-%s.log", n.ID, attempt, c))
		cmdErr, err := command(ctx, r, n.ID, repo, dir, r.Setup.Pack.Checks[c], time.Duration(n.Timeout), logPath)
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
	if err := scanForSecrets(ctx, r, l, n.Repo); err != nil {
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
// repository, and stops the run at the hard approval point policy-violation. A failure of Odin
// itself stays an error.
func failCheckpoint(r *run.Run, ev gungnir.Event, err error) error {
	var hs *hardStop
	var nf *nodeFailure
	switch {
	case errors.As(err, &hs), errors.As(err, &nf):
	case leaf.IsPolicy(err):
		hs = &hardStop{gate: gungnir.GatePolicyViolation, err: err}
	default:
		return err
	}
	ev.Name, ev.Result, ev.Commit = gungnir.EventNodeFailed, err.Error(), ""
	if lerr := r.Log(ev); lerr != nil {
		return lerr
	}
	if hs != nil {
		return hs
	}
	return nf
}

// scanForSecrets looks for secrets in what a checkpoint would commit, in the leaf's sandbox (spec
// §14). Odin's host git lists the files, so the scan covers exactly what the checkpoint commits. A
// run without a sandbox has nowhere to run the scanner, and only scripted agents. Secrets are a
// hard stop at real-secrets-or-user-data, with nothing committed; a scan that couldn't run fails
// the node, because no answer is never "clean". A leaf that breaks a sandbox rule is a
// *leaf.PolicyError, as at the checkpoint.
func scanForSecrets(ctx context.Context, r *run.Run, l *leaf.Leaf, repo string) error {
	runner, err := r.Runner(ctx, repo)
	if err != nil {
		return sandboxStart(err)
	}
	if runner == nil {
		return nil
	}
	files, err := l.Pending(ctx)
	if err != nil {
		return err
	}
	err = scan.Run(ctx, runner, files)
	var found *scan.FoundError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &found):
		return &hardStop{gate: gungnir.GateRealSecrets, err: err}
	}
	return &nodeFailure{err}
}

// readOnly refuses a read-only agent that changed the leaf: the tool flags and the adapter's
// settings keep such a role to read and search tools, and this is the check that doesn't trust them.
func readOnly(ctx context.Context, r *run.Run, n schema.Node) error {
	l, err := r.Leaf(n.Repo)
	if err != nil {
		return err
	}
	st, err := l.Git(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if st != "" {
		return &hardStop{gate: gungnir.GatePolicyViolation, err: &leaf.PolicyError{Reason: "the read-only role " + n.Role + " changed the leaf:\n" + st}}
	}
	return nil
}

// sandboxFailure is a sandbox that wouldn't start: an infrastructure failure, so the run stops
// failed with no retry loop (spec §12), and a resume tries again.
type sandboxFailure struct{ err error }

func (f *sandboxFailure) Error() string { return "the sandbox didn't start: " + f.err.Error() }
func (f *sandboxFailure) Unwrap() error { return f.err }

// sandboxStart classifies an error from starting a sandbox: a configuration that breaks the
// hardening rules is a hard stop at policy-violation; anything else is an infrastructure failure.
func sandboxStart(err error) error {
	var pe *sandbox.PolicyError
	if errors.As(err, &pe) {
		return &hardStop{gate: gungnir.GatePolicyViolation, err: err}
	}
	return &nodeFailure{&sandboxFailure{err}}
}
