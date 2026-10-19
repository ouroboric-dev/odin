// Package routing builds the routing bundle of a project umbrella (spec §8): the wiring index (the
// REPOS.md block), the impact index (.ygg/impact.json) and the bench that decides whether an index
// ships. The generators are deterministic: the same files give the same bytes, and no model is called.
// Every edge says how Odin knows it: extracted (read from a manifest, an import, an OpenAPI file or
// a lockfile) or inferred (any guess, such as matching a name).
package routing

import (
	"cmp"
	"slices"
)

// The two tags of an edge (spec §8.2).
const (
	Extracted = "extracted"
	Inferred  = "inferred"
)

// Edge is one fact of the wiring: From uses To, in the way Kind says, and Source is where Odin read it.
// A node is a leaf path (api), a contract (contract:api/internal/api.openapi.yaml), a package
// (go:github.com/x/y, npm:react, the same with @version for a pin), a realm (realm:name@version) or a
// service (service:postgres).
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Tag    string `json:"tag"`
	Source string `json:"source"`
}

func compare(a, b Edge) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.To, b.To),
		cmp.Compare(a.Tag, b.Tag), cmp.Compare(a.Source, b.Source))
}

// Sorted returns the edges in one fixed order, without exact duplicates.
func Sorted(edges []Edge) []Edge {
	out := slices.Clone(edges)
	slices.SortFunc(out, compare)
	return slices.CompactFunc(out, func(a, b Edge) bool { return compare(a, b) == 0 })
}

// Counts returns how many edges are extracted and how many are inferred.
func Counts(edges []Edge) (extracted, inferred int) {
	for _, e := range edges {
		if e.Tag == Extracted {
			extracted++
		} else {
			inferred++
		}
	}
	return extracted, inferred
}
