package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

// gallery returns the instance root and its projects/gallery umbrella, checked out.
func gallery(t *testing.T) (string, tree.Node) {
	t.Helper()
	root := treetest.Build(t, treetest.Standard())
	nodes, err := tree.Resolve(ctx, root, "projects/gallery")
	if err != nil {
		t.Fatal(err)
	}
	return root, nodes[1]
}

func blog() gungnir.Repo {
	return gungnir.Repo{Owner: "ouroboric-dev", Name: "gallery-blog", Visibility: gungnir.VisibilityPrivate,
		Description: "the gallery's blog", Path: "repos/blog",
		Files: map[string][]byte{"LICENSE": []byte("AGPL-3.0-only\n"), "LICENSES/AGPL-3.0-only.txt": []byte("...\n")}}
}

func TestLocalCreateMakesLeafWithFirstCommit(t *testing.T) {
	root, parent := gallery(t)
	before, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD")
	if err := NewLocal(parent).Create(ctx, blog()); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	nodes, err := tree.Resolve(ctx, root, "projects/gallery/repos/blog")
	if err != nil {
		t.Fatalf("the new leaf isn't in the tree: %v", err)
	}
	leaf := nodes[2]
	if leaf.Kind != schema.ChildLeaf || leaf.Owner != "ouroboric-dev" || leaf.Visibility != gungnir.VisibilityPrivate {
		t.Errorf("leaf = %+v", leaf)
	}
	repo := git.Repo{GitDir: leaf.GitDir, WorkTree: leaf.Dir}
	if files, _ := repo.Run(ctx, "ls-tree", "-r", "--name-only", "HEAD"); files != "LICENSE\nLICENSES/AGPL-3.0-only.txt" {
		t.Errorf("the first commit holds %q, want the two first-commit files and nothing else", files)
	}
	if n, _ := repo.Run(ctx, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("the leaf has %s commits, want 1", n)
	}
	if st, err := os.Stat(filepath.Join(leaf.Dir, ".git")); err != nil || st.IsDir() {
		t.Errorf("the leaf's .git must be a file pointing into the parent's .git/modules (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(leaf.GitDir, "HEAD")); err != nil {
		t.Errorf("the leaf's git dir isn't in its parent's .git/modules: %v", err)
	}
	after, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD")
	if after == before {
		t.Error("the parent umbrella has no commit recording the new leaf")
	}
	if st, _ := git.Run(ctx, parent.Dir, "status", "--porcelain"); st != "" {
		t.Errorf("the parent umbrella has uncommitted changes:\n%s", st)
	}
	if msg, _ := git.Run(ctx, parent.Dir, "log", "-1", "--format=%s"); !strings.Contains(msg, "repos/blog") {
		t.Errorf("the parent's commit message %q doesn't name the leaf", msg)
	}
}

func TestLocalCreateRefusesExistingPath(t *testing.T) {
	_, parent := gallery(t)
	plain := filepath.Join(parent.Dir, "repos", "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD")
	for _, path := range []string{"repos/api", "repos/notes"} {
		r := blog()
		r.Path = path
		if err := NewLocal(parent).Create(ctx, r); err == nil || !strings.Contains(err.Error(), "exists") {
			t.Errorf("Create(%s) = %v, want a refusal because it exists", path, err)
		}
	}
	if after, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD"); after != before {
		t.Error("a refused Create committed in the parent")
	}
	if st, _ := git.Run(ctx, parent.Dir, "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Errorf("a refused Create changed the parent:\n%s", st)
	}
	r := blog()
	r.Files = nil
	if err := NewLocal(parent).Create(ctx, r); err == nil {
		t.Error("Create() accepted a repo without first-commit files")
	}
}

func TestLocalProtectDoesNotApply(t *testing.T) {
	_, parent := gallery(t)
	before, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD")
	applied, err := NewLocal(parent).Protect(ctx, blog())
	if err != nil || applied {
		t.Errorf("Protect() = %v, %v; want false, nil: a local repo has no settings or rulesets", applied, err)
	}
	if after, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD"); after != before {
		t.Error("Protect() changed the parent")
	}
}

func TestLocalCreateRecordsTheRealmPinAndWhatTheLeafConsumesInTheSameCommit(t *testing.T) {
	_, parent := gallery(t)
	api := blog()
	api.Name, api.Path, api.Realm, api.RealmVersion = "gallery-service", "repos/service", "helheim-go", "0.2.0"
	web := blog()
	web.Name, web.Path, web.Realm, web.RealmVersion, web.Consumes = "gallery-site", "repos/site", "midgard-react", "0.1.0", []string{"repos/service"}
	l := NewLocal(parent)
	if err := l.Create(ctx, api); err != nil {
		t.Fatal(err)
	}
	if err := l.Create(ctx, web); err != nil {
		t.Fatal(err)
	}
	u, err := schema.LoadUmbrella(parent.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if c := u.Children["repos/service"]; c.Realm != "helheim-go" || c.RealmVersion != "0.2.0" || len(c.Consumes) != 0 {
		t.Errorf("api = %+v", c)
	}
	if c := u.Children["repos/site"]; c.Realm != "midgard-react" || c.RealmVersion != "0.1.0" || len(c.Consumes) != 1 || c.Consumes[0] != "repos/service" {
		t.Errorf("web = %+v", c)
	}
	if st, _ := git.Run(ctx, parent.Dir, "status", "--porcelain", "--", "umbrella.yaml"); st != "" {
		t.Errorf("umbrella.yaml is left uncommitted: %q; the pin is in the commit that creates the leaf", st)
	}
	bad := blog()
	bad.Path, bad.Name, bad.Consumes = "repos/ghost", "ghost", []string{"repos/nowhere"}
	before, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD")
	if err := l.Create(ctx, bad); err == nil {
		t.Error("a leaf that consumes a leaf the umbrella doesn't have was created")
	}
	if after, _ := git.Run(ctx, parent.Dir, "rev-parse", "HEAD"); after != before {
		t.Error("the umbrella got a commit for a leaf that was refused")
	}
	if _, err := os.Stat(filepath.Join(parent.Dir, "repos", "ghost")); err == nil {
		t.Error("a leaf that was refused left its folder behind")
	}
	if st, _ := git.Run(ctx, parent.Dir, "status", "--porcelain"); st != "" {
		t.Errorf("a refused leaf left the umbrella dirty: %q", st)
	}
}
