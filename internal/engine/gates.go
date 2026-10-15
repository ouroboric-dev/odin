package engine

import (
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
	s.Status, s.Pending = run.StatusStopped, n.ID
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

// Approve records the decision on the pending gate and moves the run past it. Advance continues.
func Approve(r *run.Run, gateID, note string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	s := r.State
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateApproved, Node: gateID, Gate: gateID, Note: note}); err != nil {
		return err
	}
	s.Pending, s.Status = "", run.StatusRunning
	s.Position++
	return r.Save()
}

// Reject sends reason back into the node the gate re-runs (spec §7.2). Advance re-runs it with
// the reason in its prompt, then reaches the gate again.
func Reject(r *run.Run, gateID, reason string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("a rejection needs a reason; it goes back to the node as input")
	}
	s := r.State
	g := r.Setup.Graph
	target := g.Nodes[g.Index(gateID)].Reruns
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateRejected, Node: gateID, Gate: gateID, Note: reason}); err != nil {
		return err
	}
	s.Reasons[target] = reason
	s.Pending, s.Status = "", run.StatusRunning
	s.Position = g.Index(target)
	return r.Save()
}
