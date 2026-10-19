package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/routing"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// shipRouting builds the routing indexes that pointer node n lists in generated, in the project umbrella
// that is the leaf's parent (spec §8): REPOS.md gets its wiring block, and .ygg/impact.json is written.
// Both are deterministic and call no model. The shrink guard then compares each with the version at HEAD
// when the pointer moves (pointer.Move), and the ledger records each index built. An umbrella that is
// not the project root ships nothing: the indexes are the project's.
func shipRouting(r *run.Run, n schema.Node, attempt int, parent tree.Node) error {
	wiring := slices.Contains(n.Generated, routing.ReposFile)
	impact := slices.Contains(n.Generated, routing.ImpactFile)
	if (!wiring && !impact) || parent.Dir != r.Project {
		return nil
	}
	packs := map[string]*schema.Pack{}
	for name, rl := range r.Setup.Realms.Realms {
		packs[name] = rl.Pack
	}
	res, err := routing.Wiring(parent.Dir, packs)
	if err != nil {
		return &nodeFailure{fmt.Errorf("routing: %w", err)}
	}
	note := ""
	if len(res.Skipped) > 0 {
		note = fmt.Sprintf("left out: %d (first: %s)", len(res.Skipped), res.Skipped[0])
	}
	// The graph pack sets the bar (spec §8.3); a leaf realm's routing block does not. The bench is in the
	// project umbrella, where no agent mounts. Under the bar, or with no bench to measure by, nothing is
	// written. A graph pack without the block ships unmeasured.
	if rp := r.Setup.GraphPack.Pack.Routing; rp != nil {
		bench, err := routing.LoadBenchIn(parent.Dir, rp.BenchPath())
		if err != nil {
			return &nodeFailure{fmt.Errorf("the routing index does not ship, because its bench can't be read: %w", err)}
		}
		rep := bench.Score(routing.IndexAnswerer(res, routing.ImpactOf(res)))
		base := bench.Score(routing.GrepBaseline(parent.Dir, res.Leaves))
		score := fmt.Sprintf("bench %v at threshold %.2f (grep baseline %v)", rep, rp.Threshold, base)
		if !rep.Ships(rp.Threshold) {
			return &nodeFailure{fmt.Errorf("the routing index does not ship: %s; wrong or unanswered: %v", score, rep.Misses)}
		}
		note = strings.TrimSpace(note + " " + score)
	}
	x, i := routing.Counts(res.Edges)
	built := func(kind string) error {
		return r.Log(gungnir.Event{Name: gungnir.EventRoutingIndexBuilt, Node: n.ID, Attempt: attempt, Task: n.Task,
			IndexKind: kind, Edges: len(res.Edges), Extracted: x, Inferred: i, Note: note})
	}
	if wiring {
		if err := routing.WriteWiring(parent.Dir, res.Edges); err != nil {
			return &nodeFailure{err}
		}
		if err := built("wiring"); err != nil {
			return err
		}
	}
	if impact {
		if err := routing.WriteImpact(parent.Dir, routing.ImpactOf(res)); err != nil {
			return &nodeFailure{err}
		}
		if err := built("impact"); err != nil {
			return err
		}
	}
	return nil
}
