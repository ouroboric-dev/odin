package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ImpactFile is the impact index, in the project umbrella (spec §8.2).
const ImpactFile = ".ygg/impact.json"

// ImpactSchema names the format of the file.
const ImpactSchema = "ygg.impact/v1"

// Reached is what a change reaches, in leaves and contracts. Extracted holds what a chain of extracted
// edges reaches. Inferred holds what only a chain with a guess in it reaches. A node is in one list.
type Reached struct {
	Extracted []string `json:"extracted"`
	Inferred  []string `json:"inferred"`
}

// ContractEntry is a contract and who reads it.
type ContractEntry struct {
	ID      string  `json:"id"`
	Path    string  `json:"path"`
	Owner   string  `json:"owner"`
	SHA256  string  `json:"sha256"`
	Readers Reached `json:"readers"` // leaves, other than the owner, that a change to the contract reaches
}

// Impact is the impact index: what a change to a leaf, a contract or a package reaches. It is a lower
// bound: it holds what manifests, imports, lockfiles and contracts show, and nothing else.
type Impact struct {
	Schema        string             `json:"schema"`
	Bound         string             `json:"bound"` // always "lower"
	Edges         int                `json:"edges"`
	Extracted     int                `json:"extracted"`
	Inferred      int                `json:"inferred"`
	Contracts     []ContractEntry    `json:"contracts"`
	Reach         map[string]Reached `json:"reach"`
	InferredEdges []Edge             `json:"inferred_edges"` // every guess, listed apart from the facts
}

// ImpactOf builds the index from a wiring. A change to X reaches every node that has an edge to X, and
// every node that has an edge to those, and so on: the edges are read backwards.
func ImpactOf(res *Result) *Impact {
	imp := &Impact{Schema: ImpactSchema, Bound: "lower", Edges: len(res.Edges), Contracts: []ContractEntry{},
		Reach: map[string]Reached{}, InferredEdges: []Edge{}}
	imp.Extracted, imp.Inferred = Counts(res.Edges)
	back := map[string][]Edge{} // node: the edges that point at it
	nodes := map[string]bool{}
	for _, e := range res.Edges {
		back[e.To] = append(back[e.To], e)
		nodes[e.To] = true
		if e.Tag == Inferred {
			imp.InferredEdges = append(imp.InferredEdges, e)
		}
	}
	reportable := map[string]bool{}
	for _, l := range res.Leaves {
		reportable[l] = true
	}
	for _, c := range res.Contracts {
		reportable["contract:"+c.Path] = true
	}
	for n := range nodes {
		if r := reach(n, back, reportable); len(r.Extracted)+len(r.Inferred) > 0 {
			imp.Reach[n] = r
		}
	}
	for _, c := range res.Contracts {
		r := reach("contract:"+c.Path, back, reportable)
		r = Reached{Extracted: leavesOnly(r.Extracted, c.Owner, res.Leaves), Inferred: leavesOnly(r.Inferred, c.Owner, res.Leaves)}
		imp.Contracts = append(imp.Contracts, ContractEntry{ID: "contract:" + c.Path, Path: c.Path, Owner: c.Owner, SHA256: c.SHA256, Readers: r})
	}
	return imp
}

func leavesOnly(ids []string, owner string, leaves []string) []string {
	out := []string{}
	for _, id := range ids {
		if id != owner && slices.Contains(leaves, id) {
			out = append(out, id)
		}
	}
	return out
}

// reach walks the edges backwards from n twice: along extracted edges only, then along all of them.
func reach(n string, back map[string][]Edge, keep map[string]bool) Reached {
	walk := func(onlyExtracted bool) map[string]bool {
		seen := map[string]bool{n: true}
		queue := []string{n}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range back[cur] {
				if (onlyExtracted && e.Tag != Extracted) || seen[e.From] {
					continue
				}
				seen[e.From] = true
				queue = append(queue, e.From)
			}
		}
		delete(seen, n)
		return seen
	}
	fact, all := walk(true), walk(false)
	r := Reached{Extracted: []string{}, Inferred: []string{}}
	for id := range all {
		switch {
		case !keep[id]:
		case fact[id]:
			r.Extracted = append(r.Extracted, id)
		default:
			r.Inferred = append(r.Inferred, id)
		}
	}
	slices.Sort(r.Extracted)
	slices.Sort(r.Inferred)
	return r
}

// Reaches returns what a change to node reaches: a leaf path, a contract id, or a package, realm or
// service node. An unknown node reaches nothing.
func (i *Impact) Reaches(node string) Reached {
	if r, ok := i.Reach[node]; ok {
		return r
	}
	return Reached{Extracted: []string{}, Inferred: []string{}}
}

// Contract returns the contract in the file at path (from the project root).
func (i *Impact) Contract(path string) (ContractEntry, bool) {
	for _, c := range i.Contracts {
		if c.Path == path {
			return c, true
		}
	}
	return ContractEntry{}, false
}

// JSON renders the index: indented, keys in a fixed order, one trailing newline.
func (i *Impact) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteImpact writes the index to dir/.ygg/impact.json.
func WriteImpact(dir string, i *Impact) error {
	b, err := i.JSON()
	if err != nil {
		return err
	}
	p := filepath.Join(dir, filepath.FromSlash(ImpactFile))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// ReadImpact reads dir/.ygg/impact.json.
func ReadImpact(dir string) (*Impact, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ImpactFile)))
	if err != nil {
		return nil, err
	}
	return ParseImpact(b)
}

// ParseImpact parses an index. It refuses an unknown field, another schema, and an index that is not
// labelled a lower bound.
func ParseImpact(b []byte) (*Impact, error) {
	var i Impact
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&i); err != nil {
		return nil, err
	}
	if i.Schema != ImpactSchema || i.Bound != "lower" {
		return nil, fmt.Errorf("impact index: schema %q and bound %q; want %q and \"lower\"", i.Schema, i.Bound, ImpactSchema)
	}
	return &i, nil
}
