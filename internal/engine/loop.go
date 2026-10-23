package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	steps := &loopSteps{r: r, n: n, st: &rec.State}
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

// loopSteps is what a loop's steps do in a run: sessions, checks, ledger lines and run.json.
type loopSteps struct {
	r  *run.Run
	n  schema.Node
	st *jormungandr.State
	// ran is how each check of the ladder ended in the attempt in progress: a dispute is decided on it.
	// The ladder runs every check again after a resume, so nothing here needs saving.
	ran map[string]checkRun
}

// checkRun is how one check ended: the attempt, whether it passed, and where its log is, under the run folder.
type checkRun struct {
	attempt int
	passed  bool
	rel     string
}

var _ jormungandr.Steps = (*loopSteps)(nil)

// prompt is the text of a session: the prompt file of the realm named realm ("" is the graph pack), the
// skills it names, and the brief. The implementer's prompt comes from the realm of the task's leaf; a
// reviewer's from the graph pack.
func (l *loopSteps) prompt(realm, file string, skillNames []string) (string, error) {
	b, err := readPrompt(l.r.Setup, realm, file)
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
	return withDecisions(text, l.r.State), nil
}

// Implement runs one implementer session with the findings that are open, and commits what it did. It
// returns the session's whole final answer, where Jormungandr looks for the implementer's disputes.
// A session that failed as an agent does leaves the leaf as the last attempt left it: what it wrote is
// discarded, so the next attempt does not build on half a thought.
func (l *loopSteps) Implement(ctx context.Context, attempt int, in jormungandr.Input) (string, error) {
	r, n := l.r, l.n
	text, err := l.prompt(n.Realm, n.Prompt, n.Skills)
	if err != nil {
		return "", err
	}
	if block := taskBlock(r, n); block != "" {
		text += "\n\n" + block
	}
	text += "\n\n" + jormungandr.FeedbackWith(in.Open, in.Rejected, in.LastFailure, r.State.Reasons[n.ID]) + jormungandr.Refusal(r.State.Refusals[n.ID])
	text = jormungandr.WithDisputeFormat(text, in.Open, n.Checks)
	started := func() error {
		return r.Log(gungnir.Event{Name: gungnir.EventLoopAttempt, Node: n.ID, Attempt: attempt,
			Result: fmt.Sprintf("attempt %d of %d starts with %d finding(s) open", attempt, l.st.Limit, len(in.Open))})
	}
	res, ev, err := session(ctx, r, sessionSpec{key: n.ID, role: n.Role, repo: n.Repo, text: text,
		maxTurns: n.MaxTurns, maxTokens: n.MaxTokens, timeout: time.Duration(n.Timeout), started: started})
	if err != nil {
		var af *jormungandr.AgentFailure
		if errors.As(err, &af) {
			if derr := r.DiscardWork(ctx, n.Repo); derr != nil {
				return "", derr
			}
		}
		return "", err
	}
	if ev.Commit, err = checkpoint(ctx, r, n, attempt); err != nil {
		return "", failCheckpoint(r, ev, err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	return res.Output, r.Log(ev)
}

// Check runs one check of the ladder in the leaf, on the work the attempt committed, and puts the leaf
// back to that commit afterwards (backlog B-004). A failing check is a finding; a check that could not
// run, because the sandbox failed under it, is an error.
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
	res, err := runCheck(ctx, r, n.ID, n.Repo, r.RepoDir(n.Repo), checks.Spec{Name: name, Argv: argv, Timeout: ExecTimeout},
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
	if l.ran == nil {
		l.ran = map[string]checkRun{}
	}
	l.ran[name] = checkRun{attempt: attempt, passed: res.Passed, rel: filepath.ToSlash(rel)}
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
	if block := taskBlock(r, n); block != "" {
		text += "\n\n" + block
	}
	text += "\n\n" + jormungandr.ReviewRequest(rv.ID, open, passed)
	before, err := snapshot(ctx, r, n.Repo)
	if err != nil {
		return "", err
	}
	sp := sessionSpec{key: n.SessionKey(rv), role: rv.Role, repo: n.Repo, text: text, readOnly: true, before: before,
		maxTurns: rv.MaxTurns, maxTokens: rv.MaxTokens, timeout: time.Duration(rv.Timeout)}
	res, ev, err := session(ctx, r, sp)
	if err := checkReadOnly(ctx, r, sp, ev, err); err != nil {
		return "", err
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	return res.Output, r.Log(ev)
}

// Dispute decides one dispute of the implementer's after the attempt's checks ran (spec §4.6). It holds only
// when the named check ran in this attempt and passed, and its log names the test. The check is the referee:
// nothing here weighs the implementer's evidence.
func (l *loopSteps) Dispute(ctx context.Context, attempt int, d gungnir.Dispute) (jormungandr.DisputeOutcome, error) {
	if !slices.Contains(l.n.Checks, d.Check) {
		return jormungandr.DisputeOutcome{Verdict: jormungandr.Unsettleable,
			Reason: fmt.Sprintf("check %q is not one of this loop's checks (%s)", d.Check, strings.Join(l.n.Checks, ", "))}, nil
	}
	not := func(format string, a ...any) (jormungandr.DisputeOutcome, error) {
		return jormungandr.DisputeOutcome{Verdict: jormungandr.DoesNotHold, Reason: fmt.Sprintf(format, a...)}, nil
	}
	got, ran := l.ran[d.Check]
	switch {
	case !ran || got.attempt != attempt:
		return not("check %s did not run in attempt %d (the ladder stops at the first check that fails)", d.Check, attempt)
	case !got.passed:
		return not("check %s did not pass in attempt %d (%s)", d.Check, attempt, got.rel)
	}
	found, err := logNames(filepath.Join(l.r.Dir, filepath.FromSlash(got.rel)), d.Test)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return not("the log of check %s in attempt %d is absent (%s)", d.Check, attempt, got.rel)
	case err != nil:
		return jormungandr.DisputeOutcome{}, &nodeFailure{fmt.Errorf("reading the log of check %s: %w", d.Check, err)}
	case !found:
		return not("check %s passed in attempt %d, but its output (%s) does not name %s", d.Check, attempt, got.rel, d.Test)
	}
	return jormungandr.DisputeOutcome{Verdict: jormungandr.Holds,
		Proof: fmt.Sprintf("check %s ran in attempt %d and passed, and its output names %s (%s)", d.Check, attempt, d.Test, got.rel)}, nil
}

// maxLogRead bounds how much of a check log a dispute reads: checks.Run keeps at most 8 MiB, and the bound
// leaves room for its truncation notice.
const maxLogRead = 9 << 20

// logNames reports whether the file at path contains test, reading it in blocks, never all at once.
func logNames(path, test string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	needle := []byte(test)
	if len(needle) == 0 {
		return false, nil
	}
	rd := bufio.NewReaderSize(io.LimitReader(f, maxLogRead), 64<<10)
	buf := make([]byte, 64<<10)
	var tail []byte // the end of the last block, so a match across two blocks is found
	for {
		n, err := rd.Read(buf)
		window := append(tail, buf[:n]...)
		if bytes.Contains(window, needle) {
			return true, nil
		}
		tail = bytes.Clone(window[max(0, len(window)-(len(needle)-1)):])
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
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
