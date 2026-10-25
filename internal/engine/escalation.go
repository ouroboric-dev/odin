package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
)

// escalation is a loop that gave up: it ran out of attempts, an implementer disputed a finding that no
// check settles, an implementer or a reviewer answered that it could not do its work, or a reviewer's answer
// could not be used twice in a row (spec §4.6, §7.3). The run stops for a person.
type escalation struct{ out jormungandr.Outcome }

func (e *escalation) Error() string {
	return "the loop gave up (" + e.out.Reason + "): " + e.out.Detail
}

// stopEscalation stops the run at the approval point escalation, as a gate node stops: a gate file with
// the findings that are still open and what the operator can do, gate_pending, and the run saved as
// stopped. The leaf stays at the last attempt's commit, so nothing is lost.
func stopEscalation(r *run.Run, n schema.Node, e *escalation) (Outcome, error) {
	s := r.State
	rel := filepath.Join("gates", fmt.Sprintf("%s-%d.md", n.ID, e.out.Attempts))
	var b strings.Builder
	fmt.Fprintf(&b, "# Escalation at loop %s (%s)\n\n", n.ID, gungnir.GateEscalation)
	fmt.Fprintf(&b, "- Run: %s\n- Mode: %s\n- Time: %s\n- Reason: %s\n\n", s.ID, s.Mode, time.Now().UTC().Format(time.RFC3339), e.out.Reason)
	// The detail holds agents' words (a blocked reason, a claim, a dispute's evidence): it can't start a heading here.
	fmt.Fprintf(&b, "## What happened\n\n%s\n\n## Findings still open\n\n", mdsafe.Text(e.out.Detail))
	if len(e.out.Open) == 0 {
		b.WriteString("None.\n")
	}
	// A finding's fields are its source's words too: each reads as its text (mdsafe).
	for _, f := range e.out.Open {
		fmt.Fprintf(&b, "- %s (%s, from %s) at %s\n", mdsafe.Line(f.ID), mdsafe.Line(string(f.Severity)), mdsafe.Line(f.Source), mdsafe.Line(f.Location))
		b.WriteString("  " + mdsafe.Item(strings.TrimSpace(f.Claim), 2) + "\n")
	}
	u := s.Usage
	fmt.Fprintf(&b, "\n## Cost so far\n\n- Tokens: %d in, %d out, %d from cache\n- Turns: %d\n- Cost: $%.4f\n", u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	b.WriteString("\n## Decide\n\n" + decideText(s.ID, n, e.out.Reason, s.Loops[n.ID].Escalation))
	if err := os.MkdirAll(filepath.Join(r.Dir, "gates"), 0o755); err != nil {
		return Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, rel), []byte(b.String()), 0o644); err != nil {
		return Outcome{}, err
	}
	ev := gungnir.Event{Name: gungnir.EventGatePending, Node: n.ID, Attempt: e.out.Attempts, Gate: n.ID, GateFile: rel,
		Note: fmt.Sprintf("%s: %s", gungnir.GateEscalation, e.out.Reason)}
	if err := r.Log(ev); err != nil {
		return Outcome{}, err
	}
	s.Status, s.Pending, s.PendingGate = run.StatusStopped, n.ID, gungnir.GateEscalation
	return Outcome{Status: run.StatusStopped, Gate: n.ID}, r.Save()
}

// decideText is the gate file's Decide section for a loop that escalated for reason: what a rejection and an
// approval do, each reason in its own words. A reviewer whose answer can't be read is offered no approval. esc is the
// loop's escalation record, which names a blocked reviewer.
func decideText(id string, n schema.Node, reason string, esc *run.Escalation) string {
	reject := fmt.Sprintf("    ygg reject %s %s --reason \"...\" | --decisions <file>\n\n", id, n.ID)
	approve := fmt.Sprintf("\n    ygg approve %s %s [--note \"...\" | --decisions <file>]\n\n", id, n.ID)
	again := fmt.Sprintf("The reason goes to the implementer's next attempt, as its rejection input, and the loop gets %d more attempts, with the findings above.\n", n.Attempts)
	switch reason {
	case jormungandr.ReasonDisputed:
		return reject + again + approve + "The implementer is right: the work goes on as it is, and the reviewer's claim above does not stop it. " +
			"The finding stays open in the ledger, with both sides: only a check or a reviewer can close one.\n"
	case jormungandr.ReasonExhausted:
		return reject + again + approve + "The work goes on as it is. The findings stay open in the ledger: only a check or a reviewer can close one.\n"
	case jormungandr.ReasonBlocked:
		return "The implementer answered that it could not do the work, for the reason above; no check or reviewer judged the attempt.\n\n" +
			reject + "The reason is the answer to what it lacked: it goes to the implementer's next attempt, and the loop gets " + fmt.Sprint(n.Attempts) +
			" more attempts.\n" + approve + "The work goes on as the blocked attempt left it, unjudged. The findings stay open in the ledger: " +
			"only a check or a reviewer can close one.\n"
	case jormungandr.ReasonReviewerBlocked:
		return "The reviewer answered that it could not review the work, twice in a row, for the reason above.\n\n" +
			reject + "The checks and the reviewer run again on the same work, and the implementer does not. The reason goes to the reviewer " +
			mdsafe.Line(blockedReviewer(esc)) + ", as its rejection input: it answers what the reviewer lacked.\n" + approve + "The work goes on as it is, without that reviewer's report. The findings stay open " +
			"in the ledger: only a check or a reviewer can close one.\n"
	}
	return reject + "The checks and the reviewer run again on the same work. The reason goes to the reviewer " + mdsafe.Line(blockedReviewer(esc)) +
		", as its rejection input.\n\nApproving is not offered: a reviewer whose answer can't be read has " +
		"not approved anything. Fix the reviewer, its role or its prompt, and then reject, to ask it again.\n"
}

// blockedReviewer is the reviewer a reviewer-blocked or reviewer-unreadable escalation names, or "that could not
// answer" for a record without one.
func blockedReviewer(esc *run.Escalation) string {
	if esc == nil || esc.Reviewer == "" {
		return "that could not answer"
	}
	return esc.Reviewer
}
