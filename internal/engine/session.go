package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// sessionSpec says what one agent session is. key names its ledger lines, its transcripts and its
// script: a node's id, or the session key of a loop's reviewer.
type sessionSpec struct {
	key      string
	role     string
	repo     string
	text     string // the whole prompt
	readOnly bool
	maxTurns int
	timeout  time.Duration
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
	var fatal error // an error of Odin's own, which must never read as a failure of the agent
	var done gungnir.Event
	meter := r.Meter()
	retrier := jormungandr.Retrier{Backoff: r.Setup.Backoff, Meter: meter, Sleep: r.Setup.Sleep,
		OnRetry: func(try int, wait time.Duration, why string) error {
			return r.Log(gungnir.Event{Name: gungnir.EventRetry, Node: sp.key, Attempt: r.State.Attempts[sp.key],
				Note: fmt.Sprintf("try %d failed for a reason that is not the agent's; it runs again in %s: %s", try, wait, why)})
		}}
	res, err := retrier.Do(ctx, func(ctx context.Context, _ int) (gungnir.Result, error) {
		stop := func(err error) (gungnir.Result, error) {
			fatal = err
			return gungnir.Result{}, &gungnir.SessionError{Kind: gungnir.FailureInfra, Err: err}
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
		runner, err := r.Runner(ctx, sp.repo)
		if err != nil {
			// A sandbox that won't start is not a failure of the agent, and is not retried here (spec §12).
			return stop(sandboxStart(err))
		}
		task := gungnir.Task{Role: sp.role, Node: sp.key, Attempt: attempt, Prompt: sp.text, Workdir: r.RepoDir(sp.repo),
			Model: role.Model, ReadOnly: sp.readOnly, MaxTurns: turnsFor(sp.maxTurns, meter.RemainingTurns()), Timeout: sp.timeout,
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
		if err := r.Spend(sp.key, spent); err != nil {
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

// turnsFor bounds a session's turns by what the caps still allow, so one session can't run far past
// a cap that is checked only between sessions. 0 means the adapter's own limit.
func turnsFor(node int, left int64) int {
	if node > 0 && int64(node) <= left {
		return node
	}
	return int(min(left, 1<<30))
}

// nodeError turns the error of a session into what the engine does with it, for a node that is not
// in a loop: a budget cap that is reached is a hard stop at budget-cap, a used-up quota pauses the run,
// and everything else that is the session's own, an agent failure, retries that ran out or an
// infrastructure failure, fails the node.
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
