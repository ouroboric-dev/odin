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
	// Phase 1 has no conformance review and no contracts, so both facts are false. Block only
	// arises for pointer-contract, which Phase 2 brings; until then it stops like Stop.
	action := gate.Decide(s.Mode, n.Gate, r.Setup.Config.Gates, gate.Facts{})
	file, err := writeGateFile(r, n, attempt, action)
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
func writeGateFile(r *run.Run, n schema.Node, attempt int, action gate.Action) (string, error) {
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
	nodes := r.Setup.Graph.Nodes
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
	n := r.Setup.Graph.Nodes[s.Position]
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateApproved, Node: gateID, Gate: gateID, Note: note}); err != nil {
		return err
	}
	switch {
	case n.Kind == schema.KindGate:
		s.Position++
	case s.PendingGate == gungnir.GatePolicyViolation:
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

// Reject sends reason back into the node the gate re-runs (spec §7.2). Advance re-runs it with
// the reason in its prompt, then reaches the gate again. A hard stop a node raised takes no
// rejection: approve it, or start a new run to drop the work.
func Reject(ctx context.Context, r *run.Run, gateID, reason string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	s := r.State
	g := r.Setup.Graph
	if n := g.Nodes[s.Position]; n.Kind != schema.KindGate {
		return fmt.Errorf("%s stopped at the hard approval point %s, which takes approve; to drop the work, start a new run", gateID, s.PendingGate)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("a rejection needs a reason; it goes back to the node as input")
	}
	target := g.Nodes[g.Index(gateID)].Reruns
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateRejected, Node: gateID, Gate: gateID, Note: reason}); err != nil {
		return err
	}
	s.Reasons[target] = reason
	s.Pending, s.PendingGate, s.Status = "", "", run.StatusRunning
	s.Position = g.Index(target)
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
	if h.gate == gungnir.GatePolicyViolation {
		fmt.Fprintf(&b, "Until then the leaf %s stays as the node left it, for investigation. Approving puts it back to its last checkpoint and runs %s again.\n", n.Repo, n.ID)
	} else {
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
