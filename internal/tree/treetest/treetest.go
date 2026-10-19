// Package treetest builds instance trees for tests: every node is a repo with its own bare
// "remote" in a temporary directory, and the root is cloned without its submodules, as an operator
// would clone a project.
package treetest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
)

// Node describes a node of a fixture tree.
type Node struct {
	Owner      string
	Visibility gungnir.Visibility
	Leaf       bool            // a leaf holds a README; an umbrella holds umbrella.yaml
	Children   map[string]Node // path relative to this umbrella: child node
	Plain      []string        // submodules that are not nodes, such as shared assets an umbrella vendors
	Pin        Pin             // a leaf's realm leaf, and its version
	GraphPin   Pin             // an umbrella's graph pack and its version (Realm holds the graph's name)
	Consumes   []string        // a leaf: the sibling leaves it calls
}

// Pin is a realm leaf and the version a fixture node pins; the zero Pin is none.
type Pin struct{ Realm, Version string }

// Standard is a company umbrella over two projects and their leaves, the shape Asgard's company
// trees have: one project is public with a private leaf, and assets/ and tools/ are submodules
// that are not nodes.
func Standard() Node {
	leaf := func(v gungnir.Visibility) Node { return Node{Owner: "ouroboric-dev", Visibility: v, Leaf: true} }
	return Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Plain: []string{"assets", "tools"},
		Children: map[string]Node{
			"projects/gallery": {Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{
				"repos/api": leaf(gungnir.VisibilityPrivate), "repos/web": leaf(gungnir.VisibilityPrivate)}},
			"projects/site": {Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPublic, Children: map[string]Node{
				"repos/www": leaf(gungnir.VisibilityPrivate)}},
		}}
}

// Deep has an extra umbrella level: company → projects/gallery → services/backend → repos/api
// (spec §3.1).
func Deep() Node {
	return Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{
		"projects/gallery": {Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{
			"services/backend": {Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{
				"repos/api": {Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Leaf: true}}}}}}}
}

// Project builds a project umbrella that is its own tree's root, with the given leaves (path from
// the project), and returns its fresh clone. The leaves stay unchecked-out, as in a fresh clone.
func Project(t *testing.T, leaves ...string) string {
	t.Helper()
	project := Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{}}
	for _, l := range leaves {
		project.Children[l] = Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Leaf: true}
	}
	return Build(t, project)
}

// Build makes the tree's repos and returns the path of a fresh clone of the root, with no
// submodule initialised.
func Build(t *testing.T, root Node) string {
	t.Helper()
	ctx := context.Background()
	remotes := t.TempDir()
	url := build(t, ctx, remotes, "root", root, true)
	work := filepath.Join(t.TempDir(), "l0")
	run(t, ctx, filepath.Dir(work), "clone", "-q", url, work)
	return work
}

// build makes node's repo and its children's, and returns the URL of its bare remote.
func build(t *testing.T, ctx context.Context, remotes, name string, n Node, root bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	run(t, ctx, t.TempDir(), "init", "-q", dir)
	if n.Leaf {
		write(t, filepath.Join(dir, "README.md"), "# "+name+"\n")
	} else {
		u := schema.Umbrella{Root: root, Owner: n.Owner, Visibility: n.Visibility, Graph: n.GraphPin.Realm, GraphVersion: n.GraphPin.Version,
			Children: map[string]schema.Child{}}
		for path, c := range n.Children {
			kind := schema.ChildUmbrella
			if c.Leaf {
				kind = schema.ChildLeaf
			}
			u.Children[path] = schema.Child{Kind: kind, Owner: c.Owner, Visibility: c.Visibility,
				Realm: c.Pin.Realm, RealmVersion: c.Pin.Version, Consumes: c.Consumes}
		}
		if err := u.Save(dir); err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, len(n.Children))
		for p := range n.Children {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			child := build(t, ctx, remotes, name+"-"+filepath.Base(p), n.Children[p], false)
			run(t, ctx, dir, "submodule", "add", "-q", child, p)
		}
		for _, p := range n.Plain {
			child := build(t, ctx, remotes, name+"-"+p, Node{Leaf: true}, false)
			run(t, ctx, dir, "submodule", "add", "-q", child, p)
		}
	}
	run(t, ctx, dir, "add", "-A")
	run(t, ctx, dir, "commit", "-q", "-m", "fixture "+name)
	bare := filepath.Join(remotes, name+".git")
	run(t, ctx, remotes, "clone", "-q", "--bare", dir, bare)
	return bare
}

func run(t *testing.T, ctx context.Context, dir string, args ...string) {
	t.Helper()
	if _, err := git.Run(ctx, dir, args...); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// SubProject builds a project that holds a sub-project, projects/sub, with the given leaves, and
// returns the root and the sub-project's directory, checked out. A project MAY hold sub-projects
// (change project-is-its-own-tree), though company-shaped work belongs in Asgard.
func SubProject(t *testing.T, leaves ...string) (root, sub string) {
	t.Helper()
	child := Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{}}
	for _, l := range leaves {
		child.Children[l] = Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Leaf: true}
	}
	root = Build(t, Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Children: map[string]Node{"projects/sub": child}})
	run(t, context.Background(), root, "submodule", "update", "--init", "--", "projects/sub")
	return root, filepath.Join(root, "projects", "sub")
}
