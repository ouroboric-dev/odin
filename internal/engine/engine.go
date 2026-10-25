// Package engine walks a run's graph (spec §4): it runs exec and agent nodes, and stops or passes
// at gate nodes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/checks"
	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gleipnir/scan"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/realm"
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
	s.Status, s.Error, s.Paused = run.StatusRunning, "", nil
	if err := r.Save(); err != nil {
		return Outcome{}, err
	}
	for {
		nodes := r.Nodes()
		if err := finishTask(r, nodes); err != nil {
			return Outcome{}, err
		}
		if moved, err := skipBlocked(r, nodes); err != nil {
			return Outcome{}, err
		} else if moved {
			continue
		}
		if err := startTask(ctx, r, nodes); err != nil {
			return Outcome{}, err
		}
		if s.Position >= len(nodes) {
			break
		}
		n := nodes[s.Position]
		var stop bool
		var err error
		switch n.Kind {
		case schema.KindAgent:
			if n.Scratch {
				err = runScratch(ctx, r, n)
			} else {
				err = runAgent(ctx, r, n)
			}
			if err == nil {
				err = ownerCheck(r, n) // a spec must cite every owner decision (ownercheck.go)
			}
			if err == nil {
				// The session's work stands: its rejection and refusal are used. A spec the check sends back keeps them,
				// so its next session still has the person's reason beside the check's refusal.
				delete(s.Reasons, n.ID)
				delete(s.Refusals, n.ID)
			}
		case schema.KindLoop:
			err = runLoop(ctx, r, n)
		case schema.KindExec:
			err = runExec(ctx, r, n)
		case schema.KindGate:
			stop, err = reachGate(ctx, r, n)
		case schema.KindMerge:
			err = runMerge(ctx, r, n)
		case schema.KindPointer:
			err = runPointer(ctx, r, n)
		case schema.KindTasks:
			err = runTasks(ctx, r, n)
		case schema.KindForeach:
			err = expandForeach(ctx, r, n)
		case schema.KindPublish:
			err = runPublish(ctx, r, n)
		case schema.KindInit:
			err = runInit(ctx, r, n)
		case schema.KindGrade:
			err = runGrade(ctx, r, n)
		default:
			// A kind the engine does not know must never pass as if it had run: stop the run on the manifest.
			err = unknownKind(r, n)
		}
		var sb *sentBack
		var ex *expanded
		if errors.As(err, &sb) || errors.As(err, &ex) {
			continue // Position already points at the node to run next
		}
		var nf *nodeFailure
		if errors.As(err, &nf) {
			return fail(r, n, nf.err)
		}
		var hs *hardStop
		if errors.As(err, &hs) {
			return stopHard(r, n, hs)
		}
		var p *pause
		if errors.As(err, &p) {
			return pauseRun(r, n, p)
		}
		var cb *contractBlock
		if errors.As(err, &cb) {
			if err := blockTask(ctx, r, nodes, n, cb.why); err != nil {
				return Outcome{}, err
			}
			continue
		}
		var esc *escalation
		if errors.As(err, &esc) {
			if blocksTasks(r, n) {
				// Unattended: give the task up and go on with the tasks that don't depend on it (spec §7.3).
				if err := blockTask(ctx, r, nodes, n, "its loop gave up ("+esc.out.Reason+"): "+esc.out.Detail+"."); err != nil {
					return Outcome{}, err
				}
				continue
			}
			return stopEscalation(r, n, esc)
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
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunFinished, Result: doneResult(s)}.WithUsage(s.Usage)); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: run.StatusDone}, r.Save()
}

// unknownKind fails the run for a node whose kind the engine has no way to run: the graph is a bad manifest.
func unknownKind(r *run.Run, n schema.Node) error {
	cause := fmt.Errorf("%s: the node kind %q is not one this Odin knows", gungnir.GradeBadManifest, n.Kind)
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeFailed, Node: n.ID, Result: cause.Error()}); err != nil {
		return err
	}
	return &nodeFailure{cause}
}

// doneResult is how a finished run is described: "done", or the tasks it gave up, listed first (spec §7.3).
func doneResult(s *run.State) string {
	var blocked []string
	for _, f := range s.Features {
		if f.Status == schema.TaskBlocked {
			blocked = append(blocked, f.ID)
		}
	}
	if len(blocked) == 0 {
		return "done"
	}
	return fmt.Sprintf("done, with %d task(s) blocked: %s", len(blocked), strings.Join(blocked, ", "))
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

// readPrompt reads the prompt file of the realm named realm ("" is the graph pack).
func readPrompt(s *run.Setup, realm, file string) ([]byte, error) {
	path, err := s.PromptFile(realm, file)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// runAgent runs agent node n in the repo it works in: one session, whose prompt promptFor builds from the node's
// declared inputs, and a checkpoint of what it did.
func runAgent(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	text, err := promptFor(r, n, nil, nil)
	if err != nil {
		return err
	}
	defer dropInputs(r, n) // on every way out, a snapshot that fails too; the session's own path drops them before the checkpoint
	var before string
	if n.ReadOnly {
		if before, err = snapshot(ctx, r, n.Repo); err != nil {
			return err
		}
	}
	sp := sessionSpec{key: n.ID, role: n.Role, repo: n.Repo, text: text, output: schema.Contract(n.Output), readOnly: n.ReadOnly, before: before,
		maxTurns: n.MaxTurns, maxTokens: n.MaxTokens, timeout: time.Duration(n.Timeout)}
	res, ev, err := sessionForOutput(ctx, r, sp)
	err = checkReadOnly(ctx, r, sp, ev, err)
	if derr := dropInputs(r, n); derr != nil && err == nil {
		err = derr
	}
	if err != nil {
		return nodeError(err)
	}
	if ev.Commit, err = checkpoint(ctx, r, n, ev.Attempt); err != nil {
		return failCheckpoint(r, ev, err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return err
	}
	s.Summaries[n.ID] = res.Summary // its rejection and refusal go once its output passes the owner check (advance)
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

// command runs args in repo, without a shell, for node, through the check runner (spec §2.1). In a
// leaf of a run with a sandbox it runs in the leaf's sandbox, which holds no credential and no host
// path but the leaf (spec §6); anywhere else, which is Odin's own generators in an umbrella and the
// whole of a run without a sandbox, it runs on the host with only PATH, HOME (the run's own directory,
// so caches never land in a repo) and LANG. Its output goes to logPath. A timeout (ExecTimeout when 0)
// or a cancel stops it: on the host by killing its whole process group, in the sandbox by killing what
// it started there (spec §6.2). cmdErr is the command's failure. err is Odin's: a sandbox that won't
// start is a *sandboxFailure or a *hardStop, and one that fails under the command is an infrastructure
// failure of the node, never a failed command (spec §12; backlog B-017).
func command(ctx context.Context, r *run.Run, node, repo, dir string, args []string, timeout time.Duration, logPath string) (cmdErr, err error) {
	res, err := runCheck(ctx, r, node, repo, dir, checks.Spec{Name: node, Argv: args, Timeout: orDefault(timeout)}, logPath)
	return commandResult(res, err, timeout)
}

// commandOn is command in a runner the caller chose, such as the snapshot sandbox of an acceptance check.
func commandOn(ctx context.Context, runner gungnir.Runner, node string, args []string, timeout time.Duration, logPath string) (cmdErr, err error) {
	res, err := runOn(ctx, runner, checks.Spec{Name: node, Argv: args, Timeout: orDefault(timeout)}, logPath)
	return commandResult(res, err, timeout)
}

// commandResult reads how a command ended: its failure as cmdErr, Odin's own failure as err.
func commandResult(res checks.Result, err error, timeout time.Duration) (cmdErr, rerr error) {
	switch {
	case err != nil:
		return nil, err
	case res.TimedOut:
		return fmt.Errorf("timed out after %s", orDefault(timeout)), nil
	case !res.Passed:
		return fmt.Errorf("exit status %d", res.ExitCode), nil
	}
	return nil, nil
}

func orDefault(timeout time.Duration) time.Duration {
	if timeout == 0 {
		return ExecTimeout
	}
	return timeout
}

// runCheck runs one command through the check runner: in repo's sandbox when it has one, on the host
// otherwise. A typed check (spec.Typed) is typed in a sandbox only: the host runner takes no environment, and
// its /tmp is shared, so a command on the host runs as it is and writes no typed result. A failure of the
// sandbox, or of the log, fails the node; a context that ended is the context's error.
func runCheck(ctx context.Context, r *run.Run, node, repo, dir string, spec checks.Spec, logPath string) (checks.Result, error) {
	runner, err := r.Runner(ctx, repo)
	if err != nil {
		return checks.Result{}, sandboxStart(err)
	}
	if runner == nil {
		runner, spec.Typed = hostRunner{r: r, node: node, dir: dir}, false
	}
	return runOn(ctx, runner, spec, logPath)
}

// runOn runs one command through the check runner in runner, and reads how it went.
func runOn(ctx context.Context, runner gungnir.Runner, spec checks.Spec, logPath string) (checks.Result, error) {
	res, err := checks.Run(ctx, runner, spec, logPath)
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		return checks.Result{}, ctx.Err()
	}
	return checks.Result{}, &nodeFailure{fmt.Errorf("the sandbox or its log failed while %s ran: %w", spec.Argv[0], err)}
}

// runChecks runs the checks names of the realm of node n in order, for the given attempt, and logs each
// result. The first failing check is the error, a *checkFailed; any other error is Odin's own. repo is the leaf
// they check, whose sandbox runs them when the run has one. The acceptance checks of a pointer node are not
// run here: they belong to every leaf's realm (runAcceptance).
func runChecks(ctx context.Context, r *run.Run, n schema.Node, attempt int, names []string, repo, dir string) error {
	rl, err := r.Setup.NodeRealm(n)
	if err != nil {
		return &nodeFailure{err}
	}
	return runChecksWith(ctx, r, n, attempt, names, rl, false, func(argv []string, logPath string) (error, error) {
		return command(ctx, r, n.ID, repo, dir, argv, time.Duration(n.Timeout), logPath)
	})
}

// runChecksWith is runChecks with the checks of realm rl, each run by exec, which returns the command's
// failure and, apart from it, Odin's own. An acceptance check (acceptance) is one of several realms': its log
// and its check_result line name the realm.
func runChecksWith(ctx context.Context, r *run.Run, n schema.Node, attempt int, names []string, rl *realm.Realm, acceptance bool, exec func(argv []string, logPath string) (cmdErr, err error)) error {
	for _, c := range names {
		argv := rl.Pack.Checks[c]
		if len(argv) == 0 {
			return &nodeFailure{fmt.Errorf("the realm %s defines no check %s", rl.Name, c)}
		}
		logName, realmName := fmt.Sprintf("%s-%d-%s.log", n.ID, attempt, c), ""
		if acceptance {
			logName, realmName = fmt.Sprintf("%s-%d-%s-%s.log", n.ID, attempt, c, rl.Name), rl.Name
		}
		logPath := filepath.Join(r.Dir, "logs", logName)
		cmdErr, err := exec(argv, logPath)
		if err != nil {
			return err
		}
		result := c + ": exit 0"
		if cmdErr != nil {
			result = c + ": " + cmdErr.Error()
		}
		if err := r.Log(gungnir.Event{Name: gungnir.EventCheckResult, Node: n.ID, Attempt: attempt, Result: result, Realm: realmName}); err != nil {
			return err
		}
		if cmdErr != nil {
			return &checkFailed{name: c, err: cmdErr, log: logPath}
		}
	}
	return nil
}

// checkFailed is a check that ran and failed: the work's failure, which a gate may send back to the
// loop that made it. Any other error of the checks is Odin's own, and fails the node (review I-5).
type checkFailed struct {
	name string
	err  error
	log  string
}

func (c *checkFailed) Error() string {
	return fmt.Sprintf("check %s failed: %v (output: %s)", c.name, c.err, c.log)
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

// snapshot lists what a leaf holds beyond its last commit, one line for each file: changed files,
// untracked files and ignored files too (backlog B-018). A read-only role gets no write tool, but a
// file it wrote that .gitignore names would not show in an ordinary status.
func snapshot(ctx context.Context, r *run.Run, repo string) (string, error) {
	l, err := r.Leaf(repo)
	if err != nil {
		return "", err
	}
	return l.Git(ctx, "status", "--porcelain", "--ignored", "-uall")
}

// readOnly refuses a read-only agent that changed the leaf: the tool flags and the adapter's settings
// keep such a role to read and search tools, and this is the check that doesn't trust them. before is
// what snapshot said before the session, since an ignored file an earlier check left is not the
// reader's doing. A file that appeared, or went, is a hard stop at policy-violation.
func readOnly(ctx context.Context, r *run.Run, repo, role, before string) error {
	after, err := snapshot(ctx, r, repo)
	if err != nil {
		return err
	}
	if after == before {
		return nil
	}
	var changed []string
	for _, side := range []struct{ from, other, mark string }{{after, before, "+ "}, {before, after, "- "}} {
		for _, line := range strings.Split(side.from, "\n") {
			if line != "" && !strings.Contains("\n"+side.other+"\n", "\n"+line+"\n") {
				changed = append(changed, side.mark+line)
			}
		}
	}
	return &hardStop{gate: gungnir.GatePolicyViolation, err: &leaf.PolicyError{Reason: "the read-only role " + role + " changed the leaf:\n" + strings.Join(changed, "\n")}}
}

// checkReadOnly is what follows a session: for a read-only one, readOnly runs whatever the session
// returned, since a session that failed may have changed the leaf too (review I1). A change, or an
// error of the check, takes precedence over the session's error, which is returned otherwise.
func checkReadOnly(ctx context.Context, r *run.Run, sp sessionSpec, ev gungnir.Event, sessionErr error) error {
	if !sp.readOnly {
		return sessionErr
	}
	if err := readOnly(context.WithoutCancel(ctx), r, sp.repo, sp.role, sp.before); err != nil {
		if sessionErr != nil { // the session's event is not returned for a failure: name the session that failed
			ev = gungnir.Event{Node: sp.key, Attempt: r.State.Attempts[sp.key], Role: sp.role}
		}
		return failCheckpoint(r, ev, err)
	}
	return sessionErr
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
