package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// Owner decisions are checked, not hoped for (change typed-boundaries, design §4): the spec must cite every decision
// the product owner gave at the gates, in one of its decisions or deviations. Odin checks it on the spec's kept output
// right after the spec node, before the conformance review. A spec that misses one goes back to the spec node once,
// with the missing ids as its refusal; a second miss stops the run at the next gate, which shows the ids. An open
// question with no answer stays allowed: the spec decides it.

// checkOwnerDecisions lists the ids of the owner decisions in ds that no decision and no deviation of spec cites, each
// once, in the order the gates gave them.
func checkOwnerDecisions(spec schema.Spec, ds []run.GateDecisions) []string {
	cited := map[string]bool{}
	for _, d := range spec.Decisions {
		for _, id := range d.Cites {
			cited[id] = true
		}
	}
	for _, d := range spec.Deviations {
		for _, id := range d.Cites {
			cited[id] = true
		}
	}
	var missing []string
	for _, g := range ds {
		for _, d := range g.Decisions.Decisions {
			if !cited[d.ID] && !slices.Contains(missing, d.ID) {
				missing = append(missing, d.ID)
			}
		}
	}
	return missing
}

// uncitedRefusal is the refusal of a spec that misses the owner decisions ids.
func uncitedRefusal(ids []string) string {
	return "the spec does not cite the product owner's decisions " + strings.Join(ids, ", ") + ": cite each in a decision or a deviation"
}

// uncited lists the owner decisions the spec node kept does not cite. A blocked spec is left to its publish, which
// stops for a person (ruling R43), and a node that kept no spec cites nothing it could miss.
func uncited(r *run.Run, node string) ([]string, error) {
	if schema.Contract(r.State.Outputs[node].Contract) != schema.ContractSpec {
		return nil, nil
	}
	spec, err := loadOutput[schema.Spec](r, node)
	if err != nil {
		return nil, err
	}
	if spec.Blocked != nil {
		return nil, nil
	}
	return checkOwnerDecisions(spec, r.State.Decisions), nil
}

// ownerCheck runs the check on the output agent node n just kept, when it is a spec. The first miss sends the spec
// back to n itself, once (s.Returns[n.ID] counts it, as for a back_to; a rejection that runs n again starts the count
// over): the refusal is n's input, and a *sentBack makes the walk run n again. A second miss lets the run go on to
// the next gate, which stops for a person with the ids (reachGate); with no gate after n, n fails.
func ownerCheck(r *run.Run, n schema.Node) error {
	if schema.Contract(n.Output) != schema.ContractSpec {
		return nil
	}
	missing, err := uncited(r, n.ID)
	if err != nil {
		return &nodeFailure{err}
	}
	if len(missing) == 0 {
		return nil
	}
	s, nodes := r.State, r.Nodes()
	if s.Returns[n.ID] >= 1 {
		at := indexOf(nodes, n.ID)
		if at >= 0 && slices.ContainsFunc(nodes[at+1:], func(m schema.Node) bool { return m.Kind == schema.KindGate }) {
			return nil
		}
		return &nodeFailure{fmt.Errorf("%s after Odin sent it back once, and no approval point follows for a person to decide", uncitedRefusal(missing))}
	}
	s.Returns[n.ID]++
	refusal := uncitedRefusal(missing)
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeReturned, Node: n.ID, Attempt: s.Returns[n.ID], Task: n.Task,
		Result: fmt.Sprintf("back to %s, 1 of 1", n.ID), Note: refusal}); err != nil {
		return err
	}
	s.Refusals[n.ID] = refusal
	if err := r.Save(); err != nil {
		return err
	}
	return &sentBack{to: n.ID}
}

// uncitedSince lists the owner decisions that a spec kept by one of nodes still does not cite: the nodes since the
// last gate, which the gate decides on.
func uncitedSince(r *run.Run, nodes []schema.Node) ([]string, error) {
	var all []string
	for _, m := range nodes {
		missing, err := uncited(r, m.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range missing {
			if !slices.Contains(all, id) {
				all = append(all, id)
			}
		}
	}
	return all, nil
}
