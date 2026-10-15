package tree

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

// initialised reports whether the submodule at path in dir is checked out.
func initialised(t *testing.T, dir, path string) bool {
	t.Helper()
	st, err := git.Run(ctx, dir, "submodule", "status", "--", path)
	if err != nil {
		t.Fatal(err)
	}
	return !strings.HasPrefix(st, "-")
}

func TestResolveChecksOutOnlyThePath(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	nodes, err := Resolve(ctx, root, "projects/gallery/repos/api")
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	var paths []string
	for _, n := range nodes {
		paths = append(paths, n.Path)
	}
	if !slices.Equal(paths, []string{"", "projects/gallery", "projects/gallery/repos/api"}) {
		t.Errorf("path = %q", paths)
	}
	leaf := nodes[2]
	if leaf.Kind != schema.ChildLeaf || leaf.Owner != "ouroboric-dev" || leaf.Dir != filepath.Join(root, "projects/gallery/repos/api") {
		t.Errorf("leaf = %+v", leaf)
	}
	if want := filepath.Join(root, ".git", "modules", "projects/gallery", "modules", "repos/api"); leaf.GitDir != want {
		t.Errorf("leaf git dir = %s, want %s (in its parent's .git/modules)", leaf.GitDir, want)
	}
	if _, err := os.Stat(filepath.Join(leaf.Dir, "README.md")); err != nil {
		t.Errorf("the leaf isn't checked out: %v", err)
	}
	for _, s := range [][2]string{{root, "projects/site"}, {root, "assets"}, {root, "tools"},
		{filepath.Join(root, "projects/gallery"), "repos/web"}} {
		if initialised(t, s[0], s[1]) {
			t.Errorf("%s was initialised; only the path to the target may be (spec §3.3 rule 6)", s[1])
		}
	}
}

func TestResolveWalksADeeperTree(t *testing.T) {
	root := treetest.Build(t, treetest.Deep())
	nodes, err := Resolve(ctx, root, "projects/gallery/services/backend/repos/api")
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	kinds := []schema.ChildKind{}
	for _, n := range nodes {
		kinds = append(kinds, n.Kind)
	}
	if !slices.Equal(kinds, []schema.ChildKind{schema.ChildUmbrella, schema.ChildUmbrella, schema.ChildUmbrella, schema.ChildLeaf}) {
		t.Errorf("kinds = %v, want three umbrellas and a leaf", kinds)
	}
	if got, rel, err := FindRoot(ctx, nodes[3].Dir); err != nil || got != root || rel != "projects/gallery/services/backend/repos/api" {
		t.Errorf("FindRoot(leaf) = %q, %q, %v", got, rel, err)
	}
}

func TestFindRoot(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	if _, err := Resolve(ctx, root, "projects/gallery"); err != nil {
		t.Fatal(err)
	}
	if got, rel, err := FindRoot(ctx, filepath.Join(root, "projects/gallery")); err != nil || got != root || rel != "projects/gallery" {
		t.Errorf("FindRoot(L1) = %q, %q, %v", got, rel, err)
	}
	if got, rel, err := FindRoot(ctx, root); err != nil || got != root || rel != "" {
		t.Errorf("FindRoot(root) = %q, %q, %v", got, rel, err)
	}
	outside := t.TempDir()
	if _, _, err := FindRoot(ctx, outside); err == nil || !strings.Contains(err.Error(), "not inside an instance tree") {
		t.Errorf("FindRoot(outside) = %v, want not inside an instance tree", err)
	}
	stray := filepath.Join(root, "projects", "stray")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := FindRoot(ctx, stray); err == nil || !strings.Contains(err.Error(), "lists no child") {
		t.Errorf("FindRoot(a directory no umbrella lists) = %v", err)
	}
}

func TestResolveRefuses(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	cases := map[string]struct{ path, want string }{
		"a path no umbrella lists":   {"projects/blog", "lists no child"},
		"a submodule that's no node": {"assets", "lists no child"},
		"a path through a leaf":      {"projects/gallery/repos/api/vendor/x", "is a leaf"},
		"a path that climbs":         {"projects/../projects/gallery", "not a clean path"},
	}
	for name, c := range cases {
		if _, err := Resolve(ctx, root, c.path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Resolve(%q) = %v, want an error containing %q", name, c.path, err, c.want)
		}
	}
	if initialised(t, root, "assets") {
		t.Error("a refused Resolve initialised assets/")
	}
}

func TestResolveRefusesAMaliciousSubmoduleURL(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	marker := filepath.Join(t.TempDir(), "pwned")
	if _, err := git.Run(ctx, root, "config", "-f", ".gitmodules", "submodule.projects/site.url", "ext::sh -c touch% "+marker); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, root, "commit", "-q", "-am", "point projects/site at a command"); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, root, "projects/site"); err == nil || !strings.Contains(err.Error(), "remote helper") {
		t.Errorf("Resolve() = %v, want the URL refused", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the URL's command ran")
	}
}

func TestResolveRefusesAChildMissingFromGitmodules(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	u, err := schema.LoadUmbrella(root)
	if err != nil {
		t.Fatal(err)
	}
	u.Children["projects/blog"] = schema.Child{Kind: schema.ChildUmbrella, Owner: "o", Visibility: "private"}
	if err := u.Save(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, root, "projects/blog"); err == nil || !strings.Contains(err.Error(), "no submodule") {
		t.Errorf("Resolve() = %v, want no submodule at projects/blog", err)
	}
}

func TestWalkVisitsInitialisedNodesAndReportsLeaks(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	if _, err := Resolve(ctx, root, "projects/site/repos/www"); err != nil {
		t.Fatal(err)
	}
	var visited, absent, leaks []string
	err := Walk(ctx, root, func(n Node) error {
		if !n.Present {
			absent = append(absent, n.Path)
			return nil
		}
		visited = append(visited, n.Path)
		if n.Umbrella != nil {
			for _, l := range n.Umbrella.Leaks() {
				leaks = append(leaks, filepath.Join(n.Path, l))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(visited, []string{"", "projects/site", "projects/site/repos/www"}) {
		t.Errorf("visited %q", visited)
	}
	if !slices.Equal(absent, []string{"projects/gallery"}) {
		t.Errorf("not checked out: %q, want projects/gallery (Walk initialises nothing)", absent)
	}
	if !slices.Equal(leaks, []string{"projects/site/repos/www"}) {
		t.Errorf("leaks = %q, want the private leaf of the public project", leaks)
	}
	if initialised(t, root, "projects/gallery") {
		t.Error("Walk initialised a submodule")
	}
}
