package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

const initNode = "  - { id: leaves, kind: init, from: docs/leaves.yaml }\n"

const galleryLeaves = `leaves:
  repos/api:
    realm: helheim-go
    realm_version: 0.2.0
    description: The gallery's API.
  repos/web:
    realm: midgard-react
    realm_version: 0.1.0
    description: The gallery's web app.
    consumes: [repos/api]
`

// initRun starts a run on a project with no leaves, and writes the leaves file into it.
func initRun(t *testing.T, leaves string, realms ...realmFiles) *run.Run {
	t.Helper()
	if realms == nil {
		realms = []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API.")}
	}
	r := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n", realms: realms})
	if err := os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(leaves), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInitCreatesEachLeafFromTheTemplateOfItsRealmAndRecordsThePin(t *testing.T) {
	r := initRun(t, galleryLeaves)
	ctx := context.Background()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	u, err := schema.LoadUmbrella(r.Project)
	if err != nil {
		t.Fatal(err)
	}
	api, web := u.Children["repos/api"], u.Children["repos/web"]
	if api.Kind != schema.ChildLeaf || api.Realm != "helheim-go" || api.RealmVersion != "0.2.0" || web.Realm != "midgard-react" || web.RealmVersion != "0.1.0" || len(web.Consumes) != 1 || web.Consumes[0] != "repos/api" {
		t.Errorf("umbrella.yaml records api %+v and web %+v", api, web)
	}
	if st, _ := git.Run(ctx, r.Project, "status", "--porcelain", "--", "umbrella.yaml", ".gitmodules"); st != "" {
		t.Errorf("the project's record of the leaves is left uncommitted: %q", st)
	}
	for leaf, realmName := range map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"} {
		l, err := r.Leaf(leaf)
		if err != nil {
			t.Fatalf("%s: %v", leaf, err)
		}
		if first, _ := l.Git(ctx, "ls-tree", "-r", "--name-only", "main"); first != "LICENSE" {
			t.Errorf("%s: main holds %q; the first commit is the recipe's first_commit files", leaf, first)
		}
		if b, _ := os.ReadFile(filepath.Join(l.Dir(), "AGENTS.md")); string(b) != "rules of "+realmName+"\n" {
			t.Errorf("%s: AGENTS.md = %q; the template of its own realm", leaf, b)
		}
		if _, err := os.Stat(filepath.Join(l.Dir(), "docs", "architecture.md")); err != nil {
			t.Errorf("%s: the template's folders are copied: %v", leaf, err)
		}
		if branch, _ := l.Git(ctx, "branch", "--show-current"); branch != run.Branch(r.State.ID) {
			t.Errorf("%s is on %q, want the run's branch", leaf, branch)
		}
		if st, _ := l.Git(ctx, "status", "--porcelain"); st != "" {
			t.Errorf("%s: the template is left uncommitted: %q", leaf, st)
		}
		if rec := r.State.Leaves[leaf]; rec.Checkpoint == rec.Base || rec.Branch == "" {
			t.Errorf("%s: record %+v; the checkpoint is the template's commit, after the base", leaf, rec)
		}
	}
	if r.Setup.RealmOf("repos/api").Name != "helheim-go" || r.Setup.RealmOf("repos/web").Name != "midgard-react" {
		t.Error("the run does not use each new leaf's realm")
	}
	if rec := r.State.Realms["helheim-go"]; rec.Version != "0.2.0" || len(rec.SHA256) != 64 {
		t.Errorf("run.json lacks the realm init loaded: %+v", r.State.Realms)
	}
	text := ledgerText(t, r)
	if strings.Count(text, `"event":"repo_created"`) != 2 || !strings.Contains(text, `"event":"realm_loaded"`) || !strings.Contains(text, `"ygg.realm.name":"helheim-go"`) {
		t.Errorf("the ledger lacks repo_created and realm_loaded lines:\n%s", text)
	}
	if got := r.State.Summaries["leaves"]; !strings.Contains(got, "created 2 leaf(s) (repos/api, repos/web)") {
		t.Errorf("summary = %q", got)
	}
	// A resume knows the realms and the leaves init added.
	id, project := r.State.ID, r.Project
	r.Close()
	again, err := run.Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer again.Close()
	if len(again.Setup.Realms.Realms) != 2 || len(again.Setup.Leaves()) != 2 {
		t.Errorf("a resumed run has realms %v and leaves %v", again.Setup.Realms.Names(), again.Setup.Leaves())
	}
}

func TestInitCreatesWhatALeafConsumesFirst(t *testing.T) {
	leaves := strings.NewReplacer("repos/api", "repos/z-api", "repos/web", "repos/a-web").Replace(galleryLeaves)
	r := initRun(t, leaves)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; repos/a-web consumes repos/z-api, which sorts after it", out, err)
	}
	u, _ := schema.LoadUmbrella(r.Project)
	if got := u.Children["repos/a-web"].Consumes; len(got) != 1 || got[0] != "repos/z-api" {
		t.Errorf("a-web consumes %v", got)
	}
}

func TestInitRefusesBeforeItCreatesAnything(t *testing.T) {
	cases := map[string]struct{ leaves, want string }{
		"a pin of the lead realm at another version": {strings.Replace(galleryLeaves, "realm_version: 0.1.0", "realm_version: 0.3.0", 1), "midgard-react is pinned at 0.1.0 by . and at 0.3.0 by repos/web"},
		"a realm the store lacks":                    {strings.Replace(galleryLeaves, "helheim-go", "asgard", 1), "asgard"},
		"a leaf that consumes a leaf nobody has":     {strings.Replace(galleryLeaves, "consumes: [repos/api]", "consumes: [repos/db]", 1), "it consumes repos/db"},
		"no leaves file":                             {"", "is not there as a regular file"},
	}
	for name, c := range cases {
		r := initRun(t, c.leaves)
		if c.leaves == "" {
			os.Remove(filepath.Join(r.Project, "docs", "leaves.yaml"))
		}
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), c.want) {
			t.Errorf("%s: Advance() = %+v, %v; want a failure mentioning %q", name, out, err, c.want)
		}
		u, _ := schema.LoadUmbrella(r.Project)
		if len(u.Children) != 0 {
			t.Errorf("%s: init created %v before refusing", name, u.Paths())
		}
	}
}

func TestInitRefusesARealmWithNoLeafRecipe(t *testing.T) {
	bare := realmFiles{name: "helheim-go", version: "0.2.0"}
	r := initRun(t, galleryLeaves, recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), bare)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the realm helheim-go offers no leaf recipe") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
	if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 0 {
		t.Errorf("init created %v for a realm that can't provide a leaf", u.Paths())
	}
}

func TestInitLeavesALeafThatExistsAsItIsAndRefusesToRepinIt(t *testing.T) {
	r := initRun(t, galleryLeaves)
	ctx := context.Background()
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	// The implementer's work on the leaf must not be undone by a second pass through init.
	api, _ := r.Leaf("repos/api")
	os.WriteFile(filepath.Join(api.Dir(), "api.txt"), []byte("work\n"), 0o644)
	os.Remove(filepath.Join(api.Dir(), "AGENTS.md"))
	if _, _, err := api.Checkpoint(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	r.State.Position = 0
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("the second Advance() = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(api.Dir(), "AGENTS.md")); err == nil {
		t.Error("init put the template back into a leaf that has history")
	}
	if got := r.State.Summaries["leaves"]; !strings.Contains(got, "created 0 leaf(s) ()") || !strings.Contains(got, "2 already there") {
		t.Errorf("summary = %q", got)
	}
	// Another realm for a leaf that exists is a change of the project, not of a run.
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(strings.Replace(galleryLeaves, "helheim-go\n    realm_version: 0.2.0", "midgard-react\n    realm_version: 0.1.0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.State.Position = 0
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the project has this leaf from helheim-go 0.2.0, and the leaves file names midgard-react 0.1.0") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}
