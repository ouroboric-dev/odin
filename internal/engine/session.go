package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// sessionSpec says what one agent session is. key names its ledger lines, its transcripts and its
// script: a node's id, or the session key of a loop's reviewer.
type sessionSpec struct {
	key       string
	role      string
	repo      string
	text      string // the whole prompt
	readOnly  bool
	before    string // a read-only session's snapshot of the leaf before it ran
	maxTurns  int
	maxTokens int
	timeout   time.Duration
	// scratch, when set, is the workspace a scratch agent works in, in place of a leaf: the session runs
	// there, in the scratch sandbox of key, and a try that failed starts again from reseed's fresh copy.
	scratch string
	reseed  func() error
	// started, when set, runs once, when the first try starts: after the meter let it, so a stop for
	// a cap or a quota before the session logs nothing that says it started (review I5).
	started func() error
}

// startSession counts a new session under key and saves it before the session runs, so a session
// that is interrupted still counts. A retry after a transient failure is a session of its own.
func startSession(r *run.Run, key string) (int, error) {
	r.State.Attempts[key]++
	return r.State.Attempts[key], r.Save()
}

// session runs one agent session the way spec §12 says. A budget cap that is reached stops it before
// it starts. What the session used is counted whether it succeeded or not (backlog B-019, B-020), in
// the day's journal and the run's usage. A failure that is not the agent's, a 429 or a crash at start,
// is retried with a backoff, not as a loop attempt. It returns the result of the session that
// succeeded and the ledger event for it, which the caller completes and logs. Any error is one of
// jormungandr's: a cap, a used-up quota, retries that ran out, an agent failure, or an infrastructure
// failure; or an error of Odin's own.
func session(ctx context.Context, r *run.Run, sp sessionSpec) (gungnir.Result, gungnir.Event, error) {
	role := r.Setup.Config.Roles[sp.role]
	var fatal error // an error of Odin's own, or a cap found at the try, which must never read as a failure of the agent
	var done gungnir.Event
	meter := r.Meter()
	retrier := jormungandr.Retrier{Backoff: r.Setup.Backoff, Meter: meter, Sleep: r.Setup.Sleep,
		OnRetry: func(try int, wait time.Duration, why string) error {
			// The failed try may have left half its work in the leaf: the retry starts from the checkpoint.
			if err := setAside(ctx, r, sp); err != nil {
				return err
			}
			return r.Log(gungnir.Event{Name: gungnir.EventRetry, Node: sp.key, Attempt: r.State.Attempts[sp.key],
				Note: fmt.Sprintf("try %d failed for a reason that is not the agent's; it runs again in %s: %s", try, wait, why)})
		}}
	res, err := retrier.Do(ctx, func(ctx context.Context, _ int) (gungnir.Result, error) {
		stop := func(err error) (gungnir.Result, error) {
			fatal = err
			return gungnir.Result{}, &gungnir.SessionError{Kind: gungnir.FailureInfra, Err: err}
		}
		if sp.started != nil {
			if err := sp.started(); err != nil {
				return stop(err)
			}
			sp.started = nil
		}
		// The caps may have no turn left though the meter let this try start: another run can spend the
		// day's last turns in between. The session then does not start, rather than run with no bound
		// (review M3); the cap reaches the caller as it is, as a cap the meter found would.
		left, hit := meter.RemainingTurns()
		if hit != nil {
			return stop(hit)
		}
		attempt, err := startSession(r, sp.key)
		if err != nil {
			return stop(err)
		}
		ev := gungnir.Event{Node: sp.key, Attempt: attempt, Role: sp.role, Adapter: role.Agent, Model: role.Model}
		begin := ev
		begin.Name = gungnir.EventNodeStarted
		if err := r.Log(begin); err != nil {
			return stop(err)
		}
		var runner gungnir.Runner
		workdir := r.RepoDir(sp.repo)
		if sp.scratch != "" {
			runner, err = r.ScratchRunner(ctx, sp.key)
			workdir = sp.scratch
		} else {
			runner, err = r.Runner(ctx, sp.repo)
		}
		if err != nil {
			// A sandbox that won't start is not a failure of the agent, and is not retried here (spec §12).
			return stop(sandboxStart(err))
		}
		task := gungnir.Task{Role: sp.role, Node: sp.key, Attempt: attempt, Prompt: sp.text, Workdir: workdir,
			Model: role.Model, ReadOnly: sp.readOnly, MaxTurns: turnsFor(sp.maxTurns, left), MaxTokens: sp.maxTokens, Timeout: sp.timeout,
			TranscriptDir: filepath.Join(r.Dir, "transcripts"), Sandbox: runner}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if task.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, task.Timeout)
		}
		began := time.Now()
		res, runErr := r.Setup.Agents[sp.role].Run(actx, task)
		cancel()
		var spent gungnir.Usage
		var se *gungnir.SessionError
		switch {
		case runErr == nil:
			if verr := res.Validate(); verr != nil {
				// Only a well-formed result's usage counts; a malformed one is never trusted.
				res, runErr = gungnir.Result{}, fmt.Errorf("the agent's result is malformed: %w", verr)
			} else {
				spent = res.Usage
			}
		case errors.As(runErr, &se):
			spent = se.Usage
		}
		if err := r.Spend(sp.key, billed(role.Agent, spent)); err != nil {
			// The journal can't record the spend, so the run stops at budget-cap (review I4). The leaf goes
			// back to its checkpoint first, as for a retry, so a resume or an approval doesn't refuse it as
			// changed while the run waited.
			if _, ok := jormungandr.IsCap(err); ok {
				from := len(r.Saved)
				if serr := setAside(ctx, r, sp); serr != nil {
					return stop(serr)
				}
				r.SetAside = slices.Clone(r.Saved[from:]) // this set-aside's notes only, for the gate
			}
			return stop(err)
		}
		if err := warnBudget(r, sp.key); err != nil {
			return stop(err)
		}
		ev = ev.WithUsage(spent)
		ev.DurationMS = time.Since(began).Milliseconds()
		if runErr == nil {
			ev.Provider, ev.CLIVersion = res.Provider, res.CLIVersion
		}
		kind, _ := jormungandr.Classify(res, runErr)
		if kind != "" {
			ev.Name, ev.Result = gungnir.EventNodeFailed, fmt.Sprintf("the agent reported %s: %s", res.Status, res.Summary)
			if runErr != nil {
				ev.Result = runErr.Error()
			}
			if err := r.Log(ev); err != nil {
				return stop(err)
			}
			return res, runErr
		}
		done = ev
		return res, nil
	})
	if fatal != nil {
		return gungnir.Result{}, gungnir.Event{}, fatal
	}
	return res, done, err
}

// setAside puts the session's leaf back to its checkpoint, what the session left saved or discarded
// as DiscardWork says. A read-only session's leaf is compared first: the reset would hide what it
// changed (review I1).
func setAside(ctx context.Context, r *run.Run, sp sessionSpec) error {
	if sp.scratch != "" {
		return sp.reseed() // a workspace holds nothing worth keeping: a try starts from a fresh one
	}
	if sp.readOnly {
		if err := readOnly(context.WithoutCancel(ctx), r, sp.repo, sp.role, sp.before); err != nil {
			return err
		}
	}
	if _, ok := r.State.Leaves[sp.repo]; ok {
		return r.DiscardWork(ctx, sp.repo)
	}
	return nil
}

// turnsFor bounds a session's turns by what the caps still allow, so one session can't run far past
// a cap that is checked only between sessions. left is above zero, since a session the caps leave no
// turn for does not start, so the bound is never 0, which a gungnir.Task reads as the adapter's own
// limit.
func turnsFor(node int, left int64) int {
	if node > 0 && int64(node) <= left {
		return node
	}
	return int(min(left, 1<<30))
}

// nodeError turns the error of a session into what the engine does with it, for a node that is not
// in a loop: a budget cap that is reached is a hard stop at budget-cap, a used-up quota pauses the run,
// and everything else that is the session's own, an agent failure, retries that ran out or an
// infrastructure failure, fails the node. A session stopped for its token limit is an agent failure.
// An error of Odin's own stays as it is.
func nodeError(err error) error {
	var af *jormungandr.AgentFailure
	var ex *jormungandr.ExhaustedError
	var q *jormungandr.QuotaError
	if c, ok := jormungandr.IsCap(err); ok {
		return &hardStop{gate: gungnir.GateBudgetCap, err: c}
	}
	if errors.As(err, &q) {
		return &pause{q}
	}
	switch {
	case errors.As(err, &af), errors.As(err, &ex):
		return &nodeFailure{err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &nodeFailure{err} // the run was interrupted: it stops failed, and a resume starts the node again
	case gungnir.FailureOf(err) == gungnir.FailureInfra:
		return &nodeFailure{err}
	}
	return err
}

// pause is a node that ended because the provider's quota is used up: the run waits, resumable, and
// says why (spec §12).
type pause struct{ q *jormungandr.QuotaError }

func (p *pause) Error() string { return p.q.Error() }

// pauseRun saves the run as paused at node n. Nothing is lost: the node starts again from the last
// checkpoint when the operator resumes the run.
func pauseRun(r *run.Run, n schema.Node, p *pause) (Outcome, error) {
	s := r.State
	s.Status, s.Paused = run.StatusPaused, &run.Pause{Node: n.ID, Reason: p.q.Reason, Since: time.Now().UTC()}
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunPaused, Node: n.ID, Attempt: s.Attempts[n.ID], Result: p.q.Reason}.WithUsage(s.Usage)); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: run.StatusPaused, Err: p.q}, r.Save()
}

// nearCap is the share of a cap at which the ledger gets a warning (spec §10.1).
const nearCap = 0.8

// warnBudget logs a budget_warning for each cap that is at least 80% used and not yet reached, once
// per run for a run's cap, and once per day for the day's.
func warnBudget(r *run.Run, node string) error {
	m := r.Meter()
	for _, n := range m.Near(nearCap) {
		key := string(n.Scope) + "/" + string(n.Dimension)
		if n.Scope == gungnir.CapDay {
			key += "/" + r.Setup.Journal.Date()
		}
		if r.State.Warned[key] {
			continue
		}
		r.State.Warned[key] = true
		note := fmt.Sprintf("the per-%s %s cap is %.0f%% used: %.4g of %.4g", n.Scope, n.Dimension, 100*n.Used/n.Limit, n.Used, n.Limit)
		if err := r.Log(gungnir.Event{Name: gungnir.EventBudgetWarning, Node: node, Note: note}); err != nil {
			return err
		}
	}
	return nil
}

// billed marks the part of a session's cost that its adapter bills to a paid API, which is what the dollar caps
// count (spec §7.4). A subscription's session has a cost figure, what it would cost on the API, and bills nothing.
func billed(adapter string, u gungnir.Usage) gungnir.Usage {
	if schema.PaidAPI(adapter) {
		u.BilledUSD = u.CostUSD
	}
	return u
}
