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
	"github.com/ouroboric-dev/odin/internal/run"
)

// escalation is a loop that gave up: it ran out of attempts, or a reviewer's answer could not be used
// twice in a row (spec §4.6, §7.3). The run stops for a person.
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
	fmt.Fprintf(&b, "## What happened\n\n%s\n\n## Findings still open\n\n", e.out.Detail)
	if len(e.out.Open) == 0 {
		b.WriteString("None.\n")
	}
	for _, f := range e.out.Open {
		fmt.Fprintf(&b, "- %s (%s, from %s) at %s\n", f.ID, f.Severity, f.Source, f.Location)
		for _, line := range strings.Split(strings.TrimSpace(f.Claim), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	u := s.Usage
	fmt.Fprintf(&b, "\n## Cost so far\n\n- Tokens: %d in, %d out, %d from cache\n- Turns: %d\n- Cost: $%.4f\n", u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	b.WriteString("\n## Decide\n\n")
	fmt.Fprintf(&b, "    ygg reject %s %s --reason \"...\"\n\nThe reason goes to the implementer, and the loop gets %d more attempts, with the findings above.\n", s.ID, n.ID, n.Attempts)
	if e.out.Reason == jormungandr.ReasonExhausted {
		fmt.Fprintf(&b, "\n    ygg approve %s %s [--note \"...\"]\n\nThe work goes on as it is. The findings stay open in the ledger: only a check or a reviewer can close one.\n", s.ID, n.ID)
	} else {
		b.WriteString("\nApproving is not offered: a reviewer whose answer can't be read has not approved anything. Fix the reviewer, its role or its prompt, and then reject, to ask it again.\n")
	}
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
