package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/checks"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/run"
)

// runLoop runs loop node n (spec §4.2): an implementer, then a ladder of checks and reviewers, with the
// findings that stay open going back to the next attempt, until the work passes or the attempts run
// out. Jormungandr decides what comes next; this file is what its steps do. The loop's state is in
// run.json, saved at every boundary, so a stop for a cap or a quota, or a crash, repeats no attempt and
// loses no finding.
func runLoop(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	cfg := loopConfig(n)
	rec := s.Loops[n.ID]
	if rec == nil {
		rec = &run.LoopRecord{State: *jormungandr.NewState(cfg)}
		s.Loops[n.ID] = rec
	}
	if rec.State.Passed {
		return nil // an earlier run of this node passed; a rejection that sends the work back clears the record
	}
	if err := recordBase(ctx, r, n, rec); err != nil {
		return err
	}
	steps := &loopSteps{r: r, n: n, rec: rec}
	out, err := jormungandr.Run(ctx, cfg, &rec.State, steps)
	if err != nil {
		return nodeError(err)
	}
	switch out.Kind {
	case jormungandr.Passed:
		delete(s.Reasons, n.ID)
		delete(s.Refusals, n.ID)
		s.Summaries[n.ID] = fmt.Sprintf("passed in attempt %d of %d with %d minor finding(s) open", out.Attempts, rec.State.Limit, len(rec.State.Book.Open()))
		if over := Overruled(&rec.State); len(over) > 0 {
			s.Summaries[n.ID] += fmt.Sprintf(" and %d overruled (ygg status lists them with both sides)", len(over))
		}
		return r.Log(gungnir.Event{Name: gungnir.EventLoopAttempt, Node: n.ID, Attempt: out.Attempts, Result: "passed"})
	case jormungandr.Capped:
		return &hardStop{gate: gungnir.GateBudgetCap, err: out.Cap}
	case jormungandr.Paused:
		return &pause{out.Quota}
	}
	// Escalated: a person decides (spec §7.3). In unattended mode the task would be marked blocked and
	// the rest would go on; a linear graph has no rest, so the run stops here in every mode, and the
	// ledger says "blocked" for unattended runs (Phase 4, decision 7).
	rec.Escalation = &run.Escalation{Reason: out.Reason, Detail: out.Detail}
	result := "stopped"
	if s.Mode == gungnir.ModeUnattended {
		result = "blocked"
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventEscalation, Node: n.ID, Attempt: out.Attempts, Result: result,
		Mode: s.Mode, Note: out.Reason + ": " + out.Detail}); err != nil {
		return err
	}
	return &escalation{out: out}
}

func loopConfig(n schema.Node) jormungandr.Config {
	cfg := jormungandr.Config{Attempts: n.Attempts, Checks: n.Checks}
	for _, rv := range n.Reviewers {
		cfg.Reviewers = append(cfg.Reviewers, rv.ID)
	}
	return cfg
}

// loopSteps is what a loop's steps do in a run: sessions, checks, ledger lines and run.json. rec is the loop's
// record in run.json: its state, the leaf's commit when it started (the task's diff, which the reviewers read,
// starts there), and how its checks last ended (the sessions' check_results).
type loopSteps struct {
	r   *run.Run
	n   schema.Node
	rec *run.LoopRecord
}

var _ jormungandr.Steps = (*loopSteps)(nil)

// Implement runs one implementer session with the findings that are open, and commits what it did. It
// returns the implementer's answer, a ygg.implement/v1 document that came through the output channel
// (sessionWithOutput: asked for once more when it does not validate, and kept); one that is not valid after that
// is an agent failure. A session that failed as an agent does leaves the leaf as the last attempt left it: what
// it wrote is discarded, so the next attempt does not build on half a thought.
func (l *loopSteps) Implement(ctx context.Context, attempt int, in jormungandr.Input) (schema.Implement, error) {
	r, n := l.r, l.n
	findings, err := implementerFindings(in)
	if err != nil {
		return schema.Implement{}, err
	}
	checks, err := checkResults(l.rec.Checks, l.rec.ChecksAttempt)
	if err != nil {
		return schema.Implement{}, err
	}
	text, err := promptFor(r, n, nil, map[string]string{schema.SourceOpenFindings: findings, schema.SourceCheckResults: checks})
	if err != nil {
		return schema.Implement{}, err
	}
	defer dropInputs(r, n) // on every way out; the session's own path drops them before the checkpoint
	text = jormungandr.WithDisputeFormat(text, in.Open, n.Checks)
	started := func() error {
		return r.Log(gungnir.Event{Name: gungnir.EventLoopAttempt, Node: n.ID, Attempt: attempt,
			Result: fmt.Sprintf("attempt %d of %d starts with %d finding(s) open", attempt, l.rec.State.Limit, len(in.Open))})
	}
	ans, res, ev, err := sessionWithOutput[schema.Implement](ctx, r, sessionSpec{key: n.ID, role: n.Role, repo: n.Repo, text: text,
		output: schema.ContractImplement, maxTurns: n.MaxTurns, maxTokens: n.MaxTokens, timeout: time.Duration(n.Timeout), started: started})
	if derr := dropInputs(r, n); derr != nil && err == nil {
		err = derr
	}
	if err != nil {
		var af *jormungandr.AgentFailure
		if errors.As(err, &af) {
			if derr := r.DiscardWork(ctx, n.Repo); derr != nil {
				return schema.Implement{}, derr
			}
		}
		return schema.Implement{}, err
	}
	if ev.Commit, err = checkpoint(ctx, r, n, attempt); err != nil {
		return schema.Implement{}, failCheckpoint(r, ev, err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return schema.Implement{}, err
	}
	return ans, nil
}

// Check runs one check of the ladder in the leaf, on the work the attempt committed, and puts the leaf
// back to that commit afterwards (backlog B-004). The check is typed (gleipnir's checks.Spec.Typed, ruling
// R28): its result (ygg.check/v1) gives one finding per failure, the tests that failed and the tests that
// passed, which settle disputes (ruling R23). A failing check always reports at least one finding. A check
// that could not run, because the sandbox failed under it, is an error. How it ended is kept in the loop's
// record, for the sessions after it.
func (l *loopSteps) Check(ctx context.Context, attempt int, name string) (jormungandr.CheckOutcome, error) {
	r, n := l.r, l.n
	rel := filepath.Join("logs", fmt.Sprintf("%s-%d-%s.log", n.ID, attempt, name))
	rl, err := r.Setup.NodeRealm(n)
	if err != nil {
		return jormungandr.CheckOutcome{}, &nodeFailure{err}
	}
	argv := rl.Pack.Checks[name]
	if len(argv) == 0 {
		return jormungandr.CheckOutcome{}, &nodeFailure{fmt.Errorf("the realm %s defines no check %s", rl.Name, name)}
	}
	res, err := runCheck(ctx, r, n.ID, n.Repo, r.RepoDir(n.Repo), checks.Spec{Name: name, Argv: argv, Timeout: ExecTimeout, Typed: true},
		filepath.Join(r.Dir, rel))
	if err == nil {
		var lf *leaf.Leaf
		if lf, err = r.Leaf(n.Repo); err == nil {
			err = tidied(ctx, lf, nil)
		}
	}
	if err != nil {
		if leaf.IsPolicy(err) { // a check rewrote the leaf's .git or planted a repository
			return jormungandr.CheckOutcome{}, &hardStop{gate: gungnir.GatePolicyViolation, err: err}
		}
		return jormungandr.CheckOutcome{}, err
	}
	result := name + ": exit 0"
	if !res.Passed {
		result = fmt.Sprintf("%s: exit status %d", name, res.ExitCode)
		if res.TimedOut {
			result = name + ": timed out"
		}
	}
	ev := gungnir.Event{Name: gungnir.EventCheckResult, Node: n.ID, Attempt: attempt, Result: result, DurationMS: res.Duration.Milliseconds()}
	if res.TypedRun && res.TypedErr != nil {
		ev.Note = firstLine("its typed result: " + res.TypedErr.Error())
	}
	if err := r.Log(ev); err != nil {
		return jormungandr.CheckOutcome{}, err
	}
	out := jormungandr.CheckOutcome{Passed: res.Passed, Check: name, Failed: res.Failed(), PassedTests: res.PassedTests(),
		Proof: fmt.Sprintf("check %s ran in attempt %d and passed (%s)", name, attempt, filepath.ToSlash(rel))}
	for _, f := range res.Findings() {
		if f.Location == res.Log+":1" { // the log, named from the run folder, as every location is relative
			f.Location = filepath.ToSlash(rel) + ":1"
		}
		out.Findings = append(out.Findings, f)
	}
	l.keepCheck(attempt, run.CheckView{Check: name, Passed: res.Passed, TimedOut: res.TimedOut, FailedTests: out.Failed, PassedTests: out.PassedTests})
	return out, nil
}

// keepCheck records how check v ended in attempt, in the loop's record: a check of a new attempt starts the list
// again, and a check run again in the same attempt (a ladder resumed after a stop) replaces its line. The ladder
// saves the record with each rung.
func (l *loopSteps) keepCheck(attempt int, v run.CheckView) {
	if l.rec.ChecksAttempt != attempt {
		l.rec.Checks, l.rec.ChecksAttempt = nil, attempt
	}
	if i := slices.IndexFunc(l.rec.Checks, func(c run.CheckView) bool { return c.Check == v.Check }); i >= 0 {
		l.rec.Checks[i] = v
		return
	}
	l.rec.Checks = append(l.rec.Checks, v)
}

// Review runs one read-only reviewer on the committed work and returns its report, a ygg.review/v1 document
// that came through the output channel (sessionWithOutput: asked for once more when it does not validate, and
// kept); one that is not valid after that is an agent failure, which the loop counts as an answer it can't use.
// The reviewer is told what it reported before and how the checks of this attempt ended, all of which passed:
// reviewers run only then. Whatever it changed in the leaf, an ignored file too, is a hard stop at
// policy-violation.
func (l *loopSteps) Review(ctx context.Context, attempt int, reviewer string, open []gungnir.Finding, _ []string) (schema.Review, error) {
	r, n := l.r, l.n
	var rv schema.Reviewer
	for _, c := range n.Reviewers {
		if c.ID == reviewer {
			rv = c
		}
	}
	findings, err := reviewerFindings(open)
	if err != nil {
		return schema.Review{}, err
	}
	checks, err := checkResults(l.rec.Checks, l.rec.ChecksAttempt)
	if err != nil {
		return schema.Review{}, err
	}
	extra := map[string]string{schema.SourceOpenFindings: findings, schema.SourceCheckResults: checks}
	if slices.Contains(sessionInputs(n, &rv), schema.SourceChange) {
		change, err := reviewChange(ctx, r, n, l.rec.Base, attempt)
		if err != nil {
			return schema.Review{}, err
		}
		extra[schema.SourceChange] = change
	}
	// The prompt is built, and its input files written, before the snapshot: they are Odin's, not the reviewer's.
	text, err := promptFor(r, n, &rv, extra)
	if err != nil {
		return schema.Review{}, err
	}
	defer dropInputs(r, n) // on every way out, a snapshot that fails too
	before, err := snapshot(ctx, r, n.Repo)
	if err != nil {
		return schema.Review{}, err
	}
	sp := sessionSpec{key: n.SessionKey(rv), role: rv.Role, repo: n.Repo, text: text, output: schema.ContractReview, readOnly: true, before: before,
		maxTurns: rv.MaxTurns, maxTokens: rv.MaxTokens, timeout: time.Duration(rv.Timeout)}
	report, res, ev, err := sessionWithOutput[schema.Review](ctx, r, sp)
	err = checkReadOnly(ctx, r, sp, ev, err)
	if derr := dropInputs(r, n); derr != nil && err == nil {
		err = derr
	}
	if err != nil {
		return schema.Review{}, err
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return schema.Review{}, err
	}
	return report, nil
}

// Record writes the changes to the findings to the ledger (spec §10.1): when each was raised, how
// many attempts each took to close, how often it came back, and which were overruled by a check.
func (l *loopSteps) Record(attempt int, changes []jormungandr.Change) error {
	for _, c := range changes {
		if c.Kind == jormungandr.DisputeIgnored {
			if err := l.r.Log(gungnir.Event{Name: gungnir.EventDisputeIgnored, Node: l.n.ID, Attempt: attempt, Note: firstLine(c.Reason)}); err != nil {
				return err
			}
			continue
		}
		ev := gungnir.Event{Node: l.n.ID, Attempt: attempt, FindingID: c.Finding.ID, FindingSource: c.Finding.Source,
			FindingSeverity: string(c.Finding.Severity), FindingOrigin: l.origin(c.Finding.Source), Note: firstLine(c.Finding.Location + ": " + c.Finding.Claim)}
		switch c.Kind {
		case jormungandr.Raised:
			ev.Name = gungnir.EventFindingRaised
		case jormungandr.Closed:
			ev.Name, ev.FindingAttempts, ev.FindingReopens = gungnir.EventFindingClosed, c.Attempts, c.Reopens
		case jormungandr.Reopened:
			ev.Name, ev.FindingReopens = gungnir.EventFindingReopened, c.Reopens
		case jormungandr.Overruled:
			ev.Name, ev.FindingAttempts, ev.FindingReopens = gungnir.EventFindingOverruled, c.Attempts, c.Reopens
			ev.Note = firstLine("overruled by check " + c.Check + ": " + ev.Note)
		}
		if err := l.r.Log(ev); err != nil {
			return err
		}
	}
	return nil
}

// Overruled lists the findings of a loop that a dispute overruled, for a person to audit.
func Overruled(st *jormungandr.State) []jormungandr.Record {
	var out []jormungandr.Record
	for _, rec := range st.Book.All() {
		if rec.Status == gungnir.FindingOverruled {
			out = append(out, rec)
		}
	}
	return out
}

// Save keeps the loop's state in run.json.
func (l *loopSteps) Save(*jormungandr.State) error { return l.r.Save() }

// firstLine is the first line of s, cut to 200 characters.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len([]rune(line)) > 200 {
		return string([]rune(line)[:200]) + "…"
	}
	return line
}

// origin says whether a finding came from one of the loop's reviewers or from a check of its ladder.
func (l *loopSteps) origin(source string) gungnir.FindingOrigin {
	if slices.ContainsFunc(l.n.Reviewers, func(r schema.Reviewer) bool { return r.ID == source }) {
		return gungnir.OriginReview
	}
	return gungnir.OriginCheck
}
