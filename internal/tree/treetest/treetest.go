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
}

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
		u := schema.Umbrella{Root: root, Owner: n.Owner, Visibility: n.Visibility, Children: map[string]schema.Child{}}
		for path, c := range n.Children {
			kind := schema.ChildUmbrella
			if c.Leaf {
				kind = schema.ChildLeaf
			}
			u.Children[path] = schema.Child{Kind: kind, Owner: c.Owner, Visibility: c.Visibility}
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
