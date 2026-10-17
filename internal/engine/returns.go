package engine

import (
	"fmt"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// sentBack is a node whose refusal went back into an earlier node: Position already points at that node
// and run.json is saved, so the walk goes on from there. It is not a failure.
type sentBack struct{ to string }

func (s *sentBack) Error() string { return "sent back to " + s.to }

// sendBack lets node n's refusal go back into the earlier node n names with back_to (spec §4.5): the merge
// gate that finds the base moved, a pointer move whose checks fail, a plan with tasks that aren't ready.
// The refusal is the target's input for its next attempt, a loop gets another full set of attempts with
// its findings, and every loop between the target and n starts over, since the work under it changes when
// the target runs again. It happens at most n.Returns times; after that, or with no back_to, it reports
// false and the node fails as it always did. It returns a *sentBack when the refusal went back.
func sendBack(r *run.Run, n schema.Node, refusal string) (bool, error) {
	s := r.State
	if n.BackTo == "" || s.Returns[n.ID] >= n.Returns {
		return false, nil
	}
	nodes := r.Nodes()
	at, to := indexOf(nodes, n.ID), indexOf(nodes, n.BackTo)
	if at < 0 || to < 0 || to >= at {
		return false, fmt.Errorf("node %s can't go back to %q: it is not an earlier node of the run", n.ID, n.BackTo)
	}
	s.Returns[n.ID]++
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeReturned, Node: n.ID, Attempt: s.Returns[n.ID], Task: n.Task,
		Result: fmt.Sprintf("back to %s, %d of %d", n.BackTo, s.Returns[n.ID], n.Returns), Note: firstLine(refusal)}); err != nil {
		return false, err
	}
	target := nodes[to]
	s.Refusals[target.ID] = refusal
	for _, m := range nodes[to+1 : at] {
		if m.Kind == schema.KindLoop {
			delete(s.Loops, m.ID)
		}
	}
	if target.Kind == schema.KindLoop {
		if rec := s.Loops[target.ID]; rec != nil {
			rec.State.Reopen(loopConfig(target))
			rec.Escalation = nil
		}
	}
	s.Position, s.Pending, s.PendingGate = to, "", ""
	if err := r.Save(); err != nil {
		return false, err
	}
	return true, &sentBack{to: target.ID}
}
