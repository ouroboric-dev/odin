package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/gate"
	"github.com/ouroboric-dev/odin/internal/run"
)

// reachGate decides gate node n (spec §7.2). Both a stop and an auto-pass write a gate file, so
// unattended decisions can be audited. A stop saves the run as stopped and reports true.
func reachGate(r *run.Run, n schema.Node) (bool, error) {
	s := r.State
	attempt, err := startAttempt(r, n)
	if err != nil {
		return false, err
	}
	// The spec gate reads the verdict of the design-conformance review (spec §7.1); no gate knows a
	// contract yet, so ContractInSpec is false. Block only arises for pointer-contract.
	var facts gate.Facts
	var verdict *gungnir.Conformance
	if n.DeviationFrom != "" {
		v, err := readConformance(r, n.DeviationFrom)
		if err != nil {
			return false, &nodeFailure{err}
		}
		verdict, facts.Deviation = &v, v.Deviation
		result := "no deviation"
		if v.Deviation {
			result = "deviation: " + firstLine(v.Amendment)
		}
		if err := r.Log(gungnir.Event{Name: gungnir.EventConformanceChecked, Node: n.DeviationFrom, Attempt: attempt, Result: result}); err != nil {
			return false, err
		}
	}
	action := gate.Decide(s.Mode, n.Gate, r.Setup.Config.Gates, facts)
	file, err := writeGateFile(r, n, attempt, action, verdict)
	if err != nil {
		return false, err
	}
	ev := gungnir.Event{Node: n.ID, Attempt: attempt, Gate: n.ID, GateFile: file}
	if action == gate.Auto {
		ev.Name = gungnir.EventGateAutoPassed
		return false, r.Log(ev)
	}
	ev.Name = gungnir.EventGatePending
	if err := r.Log(ev); err != nil {
		return false, err
	}
	s.Status, s.Pending, s.PendingGate = run.StatusStopped, n.ID, n.Gate
	return true, r.Save()
}

// writeGateFile writes gates/<node>-<attempt>.md and returns its path relative to the run.
func writeGateFile(r *run.Run, n schema.Node, attempt int, action gate.Action, verdict *gungnir.Conformance) (string, error) {
	s := r.State
	rel := filepath.Join("gates", fmt.Sprintf("%s-%d.md", n.ID, attempt))
	var b strings.Builder
	decision := "stopped for a decision"
	if action == gate.Auto {
		decision = "passed automatically"
	}
	fmt.Fprintf(&b, "# Approval point %s (%s)\n\n", n.ID, n.Gate)
	fmt.Fprintf(&b, "- Run: %s\n- Mode: %s\n- Decision: %s\n- Time: %s\n\n", s.ID, s.Mode, decision,
		time.Now().UTC().Format(time.RFC3339))
	b.WriteString("## What happened\n\n")
	nodes := r.Nodes()
	start := 0
	for i := s.Position - 1; i >= 0; i-- {
		if nodes[i].Kind == schema.KindGate {
			start = i + 1
			break
		}
	}
	for _, m := range nodes[start:s.Position] {
		fmt.Fprintf(&b, "- %s (%s, attempt %d): %s\n", m.ID, m.Kind, s.Attempts[m.ID], s.Summaries[m.ID])
	}
	if verdict != nil {
		b.WriteString("\n" + conformanceSection(*verdict))
	}
	u := s.Usage
	fmt.Fprintf(&b, "\n## Cost so far\n\n- Tokens: %d in, %d out, %d from cache\n- Turns: %d\n- Cost: $%.4f",
		u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	if u.CostEstimated {
		b.WriteString(" (estimated)")
	}
	b.WriteString("\n")
	if action != gate.Auto {
		fmt.Fprintf(&b, "\n## Decide\n\n    ygg approve %s %s [--note \"...\"]\n    ygg reject %s %s --reason \"...\"\n\n"+
			"A rejection re-runs %s with the reason.\n", s.ID, n.ID, s.ID, n.ID, n.Reruns)
	}
	if err := os.MkdirAll(filepath.Join(r.Dir, "gates"), 0o755); err != nil {
		return "", err
	}
	return rel, os.WriteFile(filepath.Join(r.Dir, rel), []byte(b.String()), 0o644)
}

func pending(r *run.Run, gateID string) error {
	if r.State.Pending == "" {
		return fmt.Errorf("run %s isn't waiting at an approval point", r.State.ID)
	}
	if r.State.Pending != gateID {
		return fmt.Errorf("run %s is waiting at %s, not %s", r.State.ID, r.State.Pending, gateID)
	}
	return nil
}

// Approve records the decision on the pending approval point, and Advance continues: past a gate
// node; or, at a hard stop a node raised, with that node again. At policy-violation the leaf goes
// back to its checkpoint first; at shrink-guard the move lands once without the guard.
func Approve(ctx context.Context, r *run.Run, gateID, note string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	s := r.State
	n := r.Nodes()[s.Position]
	if n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation {
		if rec := s.Loops[n.ID]; rec == nil || rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonExhausted {
			return fmt.Errorf("loop %s stopped because a reviewer's answer can't be read, and an answer nobody can read is not an approval; "+
				"fix the reviewer or its prompt, and then reject the stop to ask it again", n.ID)
		}
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateApproved, Node: gateID, Gate: gateID, Note: note}); err != nil {
		return err
	}
	switch {
	case n.Kind == schema.KindGate:
		s.Position++
	case n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation:
		// The operator accepts the work with its findings open. They stay open in the ledger: only their
		// source can close a finding.
		rec := s.Loops[n.ID]
		rec.State.Passed, rec.Escalation = true, nil
		s.Summaries[n.ID] = fmt.Sprintf("accepted by the operator after %d attempt(s), with %d finding(s) open", rec.State.Attempt-1, len(rec.State.Book.Open()))
		delete(s.Reasons, n.ID)
		s.Position++
	case run.Quarantines(s.PendingGate) && n.Repo == "":
		// A node that works in the project, such as a publish: nothing in a leaf to put back. The person
		// edited the workspace the stop names, and the node runs again.
	case run.Quarantines(s.PendingGate):
		rec := s.Leaves[n.Repo]
		l, err := r.Leaf(n.Repo)
		if err != nil {
			return err
		}
		if err := l.Reset(ctx, rec.Branch, rec.Checkpoint); err != nil {
			return err
		}
	case s.PendingGate == gungnir.GateShrinkGuard:
		s.Waived[n.ID] = true
	}
	s.Pending, s.PendingGate, s.Status = "", "", run.StatusRunning
	return r.Save()
}

// Rejectable reports whether the run's pending stop takes a rejection: a gate node does, and so does a
// loop that gave up. A hard stop that a node raised takes approve only, or a new run (spec §7.2).
func Rejectable(r *run.Run) bool {
	s := r.State
	if s.Pending == "" || s.Position >= len(r.Nodes()) {
		return false
	}
	n := r.Nodes()[s.Position]
	return n.Kind == schema.KindGate || (n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation)
}

// Approvable reports whether the pending stop takes an approval. A loop that gave up because a
// reviewer's answer can't be read does not: that is not a decision a person can take for the reviewer.
func Approvable(r *run.Run) bool {
	s := r.State
	if s.Pending == "" || s.Position >= len(r.Nodes()) {
		return false
	}
	n := r.Nodes()[s.Position]
	if n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation {
		rec := s.Loops[n.ID]
		return rec != nil && rec.Escalation != nil && rec.Escalation.Reason == jormungandr.ReasonExhausted
	}
	return true
}

// Reject sends reason back (spec §7.2). At a gate it goes into the node the gate re-runs: Advance
// re-runs it with the reason in its prompt, then reaches the gate again; every loop from that node up
// to the gate starts over. At a loop that gave up (spec §7.3) the reason goes to the loop's implementer,
// and the loop gets a full set of attempts again, with its findings. A hard stop a node raised takes
// no rejection: approve it, or start a new run to drop the work.
func Reject(ctx context.Context, r *run.Run, gateID, reason string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	s := r.State
	nodes := r.Nodes()
	if !Rejectable(r) {
		return fmt.Errorf("%s stopped at the hard approval point %s, which takes approve; to drop the work, start a new run", gateID, s.PendingGate)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("a rejection needs a reason; it goes back to the node as input")
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateRejected, Node: gateID, Gate: gateID, Note: reason}); err != nil {
		return err
	}
	if n := nodes[s.Position]; n.Kind == schema.KindLoop {
		rec := s.Loops[n.ID]
		rec.State.Extend(loopConfig(n))
		rec.Escalation = nil
		s.Reasons[n.ID] = reason
	} else {
		target := n.Reruns
		s.Reasons[target] = reason
		gate := s.Position
		s.Position = indexOf(nodes, target)
		// Every loop from the target up to the gate starts over: the work under a loop that passed changes
		// when a node before it runs again (review I2).
		for _, m := range nodes[s.Position:gate] {
			if m.Kind == schema.KindLoop {
				delete(s.Loops, m.ID)
			}
			delete(s.Returns, m.ID) // a rejection is a fresh start: a refusal may go back again
		}
		if err := replan(r, nodes[s.Position:gate]); err != nil {
			return err
		}
	}
	s.Pending, s.PendingGate, s.Status = "", "", run.StatusRunning
	return r.Save()
}

// hardStop is a node that stops the run at a hard approval point itself: work it left breaks a
// sandbox rule (policy-violation), or a generated file shrank (shrink-guard). Hard approval points
// stop in every mode.
type hardStop struct {
	gate gungnir.Gate
	err  error
}

func (h *hardStop) Error() string { return h.err.Error() }

// journalCap reports whether err is the per-day cap that can't be kept because the day's spend
// journal can't be read or written: raising a cap or waiting for the next day doesn't lift it.
func journalCap(err error) bool {
	c, ok := jormungandr.IsCap(err)
	return ok && c.Dimension == gungnir.CapJournal
}

// stopHard stops the run at node n for the hard approval point h, as a gate node stops: a gate file,
// gate_pending, and the run saved as stopped.
func stopHard(r *run.Run, n schema.Node, h *hardStop) (Outcome, error) {
	s := r.State
	attempt := s.Attempts[n.ID]
	rel := filepath.Join("gates", fmt.Sprintf("%s-%d.md", n.ID, attempt))
	var b strings.Builder
	fmt.Fprintf(&b, "# Hard stop at node %s (%s)\n\n", n.ID, h.gate)
	fmt.Fprintf(&b, "- Run: %s\n- Mode: %s\n- Time: %s\n\n## Why\n\n%v\n\n", s.ID, s.Mode,
		time.Now().UTC().Format(time.RFC3339), h.err)
	fmt.Fprintf(&b, "## Decide\n\n    ygg approve %s %s [--note \"...\"]\n\n", s.ID, n.ID)
	switch {
	case run.Quarantines(h.gate) && n.Repo == "":
		fmt.Fprintf(&b, "Nothing was published. The documents are in %s; remove what the scan found, and then approve: %s runs again.\n", filepath.Join(r.ScratchDir(n.From), "out"), n.ID)
	case run.Quarantines(h.gate):
		fmt.Fprintf(&b, "Until then the leaf %s stays as the node left it, for investigation. Approving puts it back to its last checkpoint and runs %s again; what the node wrote is not saved.\n", n.Repo, n.ID)
	case h.gate == gungnir.GateBudgetCap && journalCap(h.err) && len(s.PendingSpend) > 0:
		// A session's spend the journal couldn't take (review I4): the session ran, and its leaf was put back.
		c, _ := jormungandr.IsCap(h.err)
		fmt.Fprintf(&b, "The spend journal can't be written (%s), so the per-day cap can't be kept. "+
			"ygg doctor checks only today's journal file, and a pending spend goes to the file of the day it was spent: the reason above names the path that failed. "+
			"The session's usage is kept in run.json, and is written to the journal first when the run goes on. ", c.Note)
		if len(r.SetAside) == 0 {
			b.WriteString("The session left no uncommitted changes to set aside. ")
		} else {
			fmt.Fprintf(&b, "The session's uncommitted changes were set aside:\n\n- %s\n\n", strings.Join(r.SetAside, "\n- "))
		}
		fmt.Fprintf(&b, "Once the journal can be written, approve: the run runs %s again.\n", n.ID)
	case h.gate == gungnir.GateBudgetCap && journalCap(h.err):
		// The day's journal can't be read: no session started.
		fmt.Fprintf(&b, "Nothing is lost: the work so far is committed and the run can go on. The day's spend journal can't be read, so the per-day cap can't be kept. "+
			"Fix what the reason above names (ygg doctor checks the journal), and then approve: the run goes on with %s.\n", n.ID)
	case h.gate == gungnir.GateBudgetCap:
		fmt.Fprintf(&b, "Nothing is lost: the work so far is committed and the run can go on. Approving does not lift a cap that is still reached. "+
			"Raise the cap in factory.yaml (budget: per_run or per_day), or wait until the day's spend starts over, and then approve: the run goes on with %s.\n", n.ID)
	default:
		b.WriteString("Approving lands this move with the generated files as they are.\n")
	}
	b.WriteString("A hard stop takes no rejection: to drop the work, start a new run.\n")
	if err := os.MkdirAll(filepath.Join(r.Dir, "gates"), 0o755); err != nil {
		return Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, rel), []byte(b.String()), 0o644); err != nil {
		return Outcome{}, err
	}
	ev := gungnir.Event{Name: gungnir.EventGatePending, Node: n.ID, Attempt: attempt, Gate: n.ID, GateFile: rel,
		Note: fmt.Sprintf("%s: %v", h.gate, h.err)}
	if err := r.Log(ev); err != nil {
		return Outcome{}, err
	}
	s.Status, s.Pending, s.PendingGate = run.StatusStopped, n.ID, h.gate
	return Outcome{Status: run.StatusStopped, Gate: n.ID}, r.Save()
}

// indexOf returns the position of the node with the given id, or -1.
func indexOf(nodes []schema.Node, id string) int {
	for i, n := range nodes {
		if n.ID == id {
			return i
		}
	}
	return -1
}
