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
	steps := &loopSteps{r: r, n: n, st: &rec.State}
	out, err := jormungandr.Run(ctx, cfg, &rec.State, steps)
	if err != nil {
		return nodeError(err)
	}
	switch out.Kind {
	case jormungandr.Passed:
		delete(s.Reasons, n.ID)
		s.Summaries[n.ID] = fmt.Sprintf("passed in attempt %d of %d with %d minor finding(s) open", out.Attempts, rec.State.Limit, len(rec.State.Book.Open()))
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

// loopSteps is what a loop's steps do in a run: sessions, checks, ledger lines and run.json.
type loopSteps struct {
	r  *run.Run
	n  schema.Node
	st *jormungandr.State
}

var _ jormungandr.Steps = (*loopSteps)(nil)

// prompt is the text of a session: the prompt file of the realm named realm ("" is the lead realm), the
// skills it names, and the brief. The implementer's prompt comes from the realm of the task's leaf; a
// reviewer's from the lead realm.
func (l *loopSteps) prompt(realm, file string, skillNames []string) (string, error) {
	b, err := os.ReadFile(filepath.Join(l.r.Setup.Realm(realm).Dir, file))
	if err != nil {
		return "", err
	}
	text := string(b)
	skillText, err := l.r.Setup.Libs.Load(skillNames)
	if err != nil {
		return "", err
	}
	if skillText != "" {
		text += "\n\n" + skillText
	}
	if l.r.Setup.BriefText != "" {
		text += "\n\n## Brief\n\n" + l.r.Setup.BriefText
	}
	return text, nil
}

// Implement runs one implementer session with the findings that are open, and commits what it did.
// A session that failed as an agent does leaves the leaf as the last attempt left it: what it wrote is
// discarded, so the next attempt does not build on half a thought.
func (l *loopSteps) Implement(ctx context.Context, attempt int, in jormungandr.Input) error {
	r, n := l.r, l.n
	text, err := l.prompt(n.Realm, n.Prompt, n.Skills)
	if err != nil {
		return err
	}
	text += "\n\n" + jormungandr.Feedback(in.Open, in.LastFailure, r.State.Reasons[n.ID])
	started := func() error {
		return r.Log(gungnir.Event{Name: gungnir.EventLoopAttempt, Node: n.ID, Attempt: attempt,
			Result: fmt.Sprintf("attempt %d of %d starts with %d finding(s) open", attempt, l.st.Limit, len(in.Open))})
	}
	res, ev, err := session(ctx, r, sessionSpec{key: n.ID, role: n.Role, repo: n.Repo, text: text,
		maxTurns: n.MaxTurns, timeout: time.Duration(n.Timeout), started: started})
	if err != nil {
		var af *jormungandr.AgentFailure
		if errors.As(err, &af) {
			if derr := r.DiscardWork(ctx, n.Repo); derr != nil {
				return derr
			}
		}
		return err
	}
	if ev.Commit, err = checkpoint(ctx, r, n, attempt); err != nil {
		return failCheckpoint(r, ev, err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	return r.Log(ev)
}

// Check runs one check of the ladder in the leaf, on the work the attempt committed, and puts the leaf
// back to that commit afterwards (backlog B-004). A failing check is a finding; a check that could not
// run, because the sandbox failed under it, is an error.
func (l *loopSteps) Check(ctx context.Context, attempt int, name string) (jormungandr.CheckOutcome, error) {
	r, n := l.r, l.n
	rel := filepath.Join("logs", fmt.Sprintf("%s-%d-%s.log", n.ID, attempt, name))
	res, err := runCheck(ctx, r, n.ID, n.Repo, r.RepoDir(n.Repo), checks.Spec{Name: name, Argv: r.Setup.Realm(n.Realm).Pack.Checks[name], Timeout: ExecTimeout},
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
	if err := r.Log(gungnir.Event{Name: gungnir.EventCheckResult, Node: n.ID, Attempt: attempt, Result: result,
		DurationMS: res.Duration.Milliseconds()}); err != nil {
		return jormungandr.CheckOutcome{}, err
	}
	out := jormungandr.CheckOutcome{Passed: res.Passed, FindingID: checks.FindingID(name),
		Proof: fmt.Sprintf("check %s ran in attempt %d and passed (%s)", name, attempt, filepath.ToSlash(rel))}
	if f, failed := res.Finding(filepath.ToSlash(rel) + ":1"); failed {
		out.Finding = f
	}
	return out, nil
}

// Review runs one read-only reviewer on the committed work and returns its whole answer, which
// Jormungandr reads. The reviewer is told what it reported before and which checks passed. Whatever
// it changed in the leaf, an ignored file too, is a hard stop at policy-violation.
func (l *loopSteps) Review(ctx context.Context, attempt int, reviewer string, open []gungnir.Finding, passed []string) (string, error) {
	r, n := l.r, l.n
	var rv schema.Reviewer
	for _, c := range n.Reviewers {
		if c.ID == reviewer {
			rv = c
		}
	}
	text, err := l.prompt("", rv.Prompt, rv.Skills)
	if err != nil {
		return "", err
	}
	text += "\n\n" + jormungandr.ReviewRequest(rv.ID, open, passed)
	before, err := snapshot(ctx, r, n.Repo)
	if err != nil {
		return "", err
	}
	sp := sessionSpec{key: n.SessionKey(rv), role: rv.Role, repo: n.Repo, text: text, readOnly: true, before: before,
		maxTurns: rv.MaxTurns, timeout: time.Duration(rv.Timeout)}
	res, ev, err := session(ctx, r, sp)
	if err := checkReadOnly(ctx, r, sp, ev, err); err != nil {
		return "", err
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	return res.Output, r.Log(ev)
}

// Record writes the changes to the findings to the ledger (spec §10.1): when each was raised, how
// many attempts each took to close, and how often it came back.
func (l *loopSteps) Record(attempt int, changes []jormungandr.Change) error {
	for _, c := range changes {
		ev := gungnir.Event{Node: l.n.ID, Attempt: attempt, FindingID: c.Finding.ID, FindingSource: c.Finding.Source,
			FindingSeverity: string(c.Finding.Severity), Note: firstLine(c.Finding.Location + ": " + c.Finding.Claim)}
		switch c.Kind {
		case jormungandr.Raised:
			ev.Name = gungnir.EventFindingRaised
		case jormungandr.Closed:
			ev.Name, ev.FindingAttempts, ev.FindingReopens = gungnir.EventFindingClosed, c.Attempts, c.Reopens
		case jormungandr.Reopened:
			ev.Name, ev.FindingReopens = gungnir.EventFindingReopened, c.Reopens
		}
		if err := l.r.Log(ev); err != nil {
			return err
		}
	}
	return nil
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
