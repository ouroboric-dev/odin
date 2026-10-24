// Package plan turns a graph's task shape into the nodes of a run (spec §4.2: "for each ready
// task"). A foreach node in the graph is written once; Expand makes its nodes for each task, in the leaf
// the task names, with the prompt and checks of the realm that provides that leaf.
package plan

import (
	"fmt"
	"slices"

	"github.com/ouroboric-dev/gungnir/schema"
)

// Recipe is what a leaf's realm offers its tasks (the pack's leaf recipe): the realm's name, the
// implementer's prompt and the checks, in the realm's own terms.
type Recipe struct {
	Realm       string
	Implementer string
	Checks      []string
}

// Expand returns the nodes of foreach node fe for tasks, which are in the order they are built: fe's body
// once for each task. A task's nodes work in its layer, which is a leaf of the project; their ids are the
// task's id as a node id, a dash and the body node's id; they name the task; a gate re-runs, a merge or
// pointer node goes back to, and an input names, the task's own node. A loop, merge or pointer node of the realm "leaf"
// takes its prompt and checks from recipeOf(layer). Expand changes nothing in the graph, and refuses an
// id or a reviewer session key that the graph already uses, or that two tasks would share.
func Expand(fe schema.Node, tasks []schema.Feature, g *schema.Graph, recipeOf func(layer string) (Recipe, error)) ([]schema.Node, error) {
	taken := map[string]bool{}
	for _, n := range g.Nodes {
		taken[n.ID] = true
		if n.Kind == schema.KindLoop {
			for _, rv := range n.Reviewers {
				taken[n.SessionKey(rv)] = true
			}
		}
	}
	inBody := map[string]bool{}
	for _, b := range fe.Body {
		inBody[b.ID] = true
	}
	var out []schema.Node
	for _, f := range tasks {
		rec, err := recipeOf(f.Layer)
		if err != nil {
			return nil, fmt.Errorf("task %s works in %s: %w", f.ID, f.Layer, err)
		}
		slug := f.Slug()
		name := func(id string) string { return slug + "-" + id }
		// An input that names a body node names this task's copy of it, whose output is kept under that id.
		inputs := func(in []string) []string {
			renamed := slices.Clone(in)
			for i, x := range renamed {
				if inBody[x] {
					renamed[i] = name(x)
				}
			}
			return renamed
		}
		for _, b := range fe.Body {
			n := b
			n.ID, n.Task, n.Repo = name(b.ID), slug, f.Layer
			if b.Reruns != "" {
				n.Reruns = name(b.Reruns)
			}
			if b.BackTo != "" {
				n.BackTo = name(b.BackTo)
			}
			n.Skills, n.Acceptance, n.Generated = slices.Clone(b.Skills), slices.Clone(b.Acceptance), slices.Clone(b.Generated)
			n.Reviewers = slices.Clone(b.Reviewers)
			n.Input = inputs(b.Input)
			for i := range n.Reviewers {
				n.Reviewers[i].Input = inputs(b.Reviewers[i].Input)
			}
			if inBody[b.Reviews] {
				n.Reviews = name(b.Reviews)
			}
			if b.RealmFrom == schema.RealmLeaf {
				if rec.Implementer == "" || len(rec.Checks) == 0 {
					return nil, fmt.Errorf("task %s: the realm %s offers no leaf recipe, so it can't provide %s", f.ID, rec.Realm, f.Layer)
				}
				n.Realm = rec.Realm
				n.Checks = slices.Clone(rec.Checks)
				if b.Kind == schema.KindLoop {
					n.Prompt = rec.Implementer
				}
			}
			keys := []string{n.ID}
			if n.Kind == schema.KindLoop {
				for _, rv := range n.Reviewers {
					keys = append(keys, n.SessionKey(rv))
				}
			}
			for _, k := range keys {
				if taken[k] {
					return nil, fmt.Errorf("task %s: the node id %q is already used in the graph, or by another task", f.ID, k)
				}
				taken[k] = true
			}
			out = append(out, n)
		}
	}
	return out, nil
}
