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
		// The template is part of main, so a first task starts from it (final review I-2): the checks'
		// "commit the task started from" is the template, and a blocked task's reset keeps it.
		rec := r.State.Leaves[leaf]
		if main, _ := l.Rev(ctx, "main"); main != rec.Checkpoint {
			t.Errorf("%s: main is at %s, want the template checkpoint %s", leaf, main, rec.Checkpoint)
		}
		if mb, _ := l.Git(ctx, "merge-base", "HEAD", "main"); mb != rec.Checkpoint {
			t.Errorf("%s: merge-base HEAD main = %s, want the template checkpoint %s", leaf, mb, rec.Checkpoint)
		}
		if tree, _ := l.Git(ctx, "ls-tree", "-r", "--name-only", "main"); !strings.Contains(tree, "LICENSE") || !strings.Contains(tree, "AGENTS.md") {
			t.Errorf("%s: main holds %q; the first commit's files and the template", leaf, tree)
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
	apiRealm, _ := r.Setup.RealmOf("repos/api")
	webRealm, _ := r.Setup.RealmOf("repos/web")
	if apiRealm == nil || webRealm == nil || apiRealm.Name != "helheim-go" || webRealm.Name != "midgard-react" {
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
		"a pin at another version than the store holds": {strings.Replace(galleryLeaves, "realm_version: 0.1.0", "realm_version: 0.3.0", 1), "the realm store holds midgard-react 0.1.0, but repos/web pins 0.3.0"},
		"a realm the store lacks":                       {strings.Replace(galleryLeaves, "helheim-go", "asgard", 1), "asgard"},
		"a leaf that consumes a leaf nobody has":        {strings.Replace(galleryLeaves, "consumes: [repos/api]", "consumes: [repos/db]", 1), "it consumes repos/db"},
		"no leaves file":                                {"", "is not there as a regular file"},
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

func TestAnInitThatStopsHalfWayCanBeResumedAndFinishes(t *testing.T) {
	r := initRun(t, galleryLeaves)
	ctx := context.Background()
	// repos/web is taken by a folder the project doesn't list, so creating it is refused after repos/api is made.
	if err := os.MkdirAll(filepath.Join(r.Project, "repos", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "exists") {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 1 {
		t.Fatalf("the project lists %v; repos/api should be there alone", u.Paths())
	}
	id, project := r.State.ID, r.Project
	r.Close()
	if err := os.Remove(filepath.Join(project, "repos", "web")); err != nil {
		t.Fatal(err)
	}
	again, err := run.Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v; the run loaded helheim-go before it stopped, and a resume keeps it", err)
	}
	defer again.Close()
	if got := strings.Join(again.Setup.Realms.Names(), ","); got != "helheim-go,midgard-react" {
		t.Errorf("realms after the resume = %s", got)
	}
	if out, err := Advance(ctx, again); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after the resume = %+v, %v", out, err)
	}
	if u, _ := schema.LoadUmbrella(project); len(u.Children) != 2 || u.Children["repos/web"].Realm != "midgard-react" {
		t.Errorf("the project lists %v after init finished", u.Paths())
	}
}

func TestARefusedLeavesFileDoesNotMakeTheRunTakeItsRealms(t *testing.T) {
	r := initRun(t, strings.Replace(galleryLeaves, "consumes: [repos/api]", "consumes: [repos/db]", 1))
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(r.State.Realms) != 1 || len(r.Setup.Realms.Realms) != 1 {
		t.Errorf("run.json holds %v after a leaves file that was refused; the run took no realm", r.State.Realms)
	}
}

func TestInitRefusesALeafInsideOrHoldingAChildTheProjectHasBeforeItCreatesAnything(t *testing.T) {
	// aa/api sorts first, so a refusal that came from the remote, leaf by leaf, would come after it was made.
	first := "leaves:\n  aa/api:\n    realm: helheim-go\n    realm_version: 0.2.0\n    description: The API.\n"
	for name, path := range map[string]string{"inside a leaf": "repos/app/sub", "holding a leaf": "repos"} {
		r := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n",
			realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API.")},
			leaves: map[string]string{"repos/app": "midgard-react"}})
		leaves := first + "  " + path + ":\n    realm: midgard-react\n    realm_version: 0.1.0\n    description: The web app.\n"
		if err := os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(leaves), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), path+": it would be inside repos/app or hold it") ||
			!strings.Contains(out.Err.Error(), "nothing was created") {
			t.Errorf("%s: Advance() = %+v, %v; want a refusal of %s", name, out, err, path)
		}
		if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 1 {
			t.Errorf("%s: the project lists %v; init created leaves before refusing", name, u.Paths())
		}
	}
}

// The leaves file is read by init and by the tasks node through one check: a regular file, reached with no
// symlink, of at most maxLeavesFile bytes. A longer one is refused, never parsed cut short (review M-3).
func TestTheLeavesFileIsARegularFileWithinTheLimitForInitAndForTheTasksNode(t *testing.T) {
	r := initRun(t, galleryLeaves)
	file := filepath.Join(r.Project, "docs", "leaves.yaml")
	initNode := schema.Node{ID: "leaves", Kind: schema.KindInit, From: "docs/leaves.yaml"}
	tasksNode := schema.Node{ID: "ready", Kind: schema.KindTasks, From: "tasks.md", Leaves: "docs/leaves.yaml"}
	check := func(name, want string) {
		t.Helper()
		_, ierr := readLeaves(r, initNode)
		_, refusal, perr := planLeaves(r, tasksNode)
		if want == "" {
			if ierr != nil || refusal != "" || perr != nil {
				t.Errorf("%s: init: %v; tasks: %q, %v", name, ierr, refusal, perr)
			}
			return
		}
		if ierr == nil || !strings.Contains(ierr.Error(), want) {
			t.Errorf("%s: init read it: %v; want a refusal mentioning %q", name, ierr, want)
		}
		if perr != nil || !strings.Contains(refusal, want) {
			t.Errorf("%s: the tasks node read it: %q, %v; want a refusal mentioning %q", name, refusal, perr, want)
		}
	}
	check("a normal file", "")
	// Valid YAML at any cut: what is past the limit is a comment, so a file read cut short would parse.
	long := galleryLeaves + "# " + strings.Repeat("x", maxLeavesFile) + "\n"
	if err := os.WriteFile(file, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	check("a file over the limit", "more than 1048576 bytes")
	os.Remove(file)
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "real.yaml"), []byte(galleryLeaves), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", file); err != nil {
		t.Fatal(err)
	}
	check("a symlink", "symlink")
	os.Remove(file)
	if err := os.MkdirAll(file, 0o755); err != nil {
		t.Fatal(err)
	}
	check("a folder", "not a regular file")
}

// galleryRealms are the two realms the leaves file names, each with a leaf recipe.
func galleryRealms() []realmFiles {
	return []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API.")}
}

// The realm's template goes only into a leaf this run's init created. A leaf the project has already is
// left as it is, even one with a single commit, which is what a leaf just created also looks like (review
// M-8).
func TestInitLeavesAnExistingLeafWithOneCommitUntouched(t *testing.T) {
	r := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n", realms: galleryRealms(), leaves: map[string]string{"repos/api": "helheim-go"}})
	os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755)
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(galleryLeaves), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	api := filepath.Join(r.Project, "repos", "api")
	if count, _ := git.Run(ctx, api, "rev-list", "--count", "HEAD"); count != "1" {
		t.Fatalf("the existing leaf has %s commits; the test needs one", count)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(api, "AGENTS.md")); err == nil {
		t.Error("init wrote the realm's template into a leaf the project had already")
	}
	if count, _ := git.Run(ctx, api, "rev-list", "--count", "HEAD"); count != "1" {
		t.Errorf("the existing leaf has %s commits after init; it was left as it was with 1", count)
	}
	web, _ := r.Leaf("repos/web")
	if _, err := os.Stat(filepath.Join(web.Dir(), "AGENTS.md")); err != nil {
		t.Errorf("the leaf init created lacks its template: %v", err)
	}
	if got := r.State.Summaries["leaves"]; !strings.Contains(got, "created 1 leaf(s) (repos/web)") || !strings.Contains(got, "1 already there (repos/api)") {
		t.Errorf("summary = %q", got)
	}
}

// halfInit runs init, then puts the run and repos/api back where a crash between creating the leaf and
// committing its template leaves them: init running, the leaf on its first commit, and run.json as init
// saved it. It returns the run, reopened as a resume does.
func halfInit(t *testing.T) *run.Run {
	t.Helper()
	ctx := context.Background()
	r := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n", realms: galleryRealms()})
	os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755)
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(galleryLeaves), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	api, _ := r.Leaf("repos/api")
	rec := r.State.Leaves["repos/api"]
	if _, err := api.Git(ctx, "reset", "-q", "--hard", rec.Base); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Git(ctx, "update-ref", "refs/heads/main", rec.Base); err != nil { // init had not moved main yet
		t.Fatal(err)
	}
	rec.Checkpoint = rec.Base
	r.State.Leaves["repos/api"] = rec
	r.State.Position, r.State.Status = 0, run.StatusRunning
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	id, project := r.State.ID, r.Project
	r.Close()
	again, err := run.Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { again.Close() })
	return again
}

// A resume of an init that stopped after creating a leaf and before committing its template still adds
// the template: run.json says this run created the leaf.
func TestAResumedInitStillAddsTheTemplateToALeafItCreated(t *testing.T) {
	r := halfInit(t)
	ctx := context.Background()
	api, _ := r.Leaf("repos/api")
	if _, err := os.Stat(filepath.Join(api.Dir(), "AGENTS.md")); err == nil {
		t.Fatal("the rewound leaf still holds its template")
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after the resume = %+v, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(api.Dir(), "AGENTS.md")); err != nil || string(b) != "rules of helheim-go\n" {
		t.Errorf("AGENTS.md = %q, %v; the resumed init adds the template of the leaf it created", b, err)
	}
	if count, _ := api.Git(ctx, "rev-list", "--count", "HEAD"); count != "2" {
		t.Errorf("the leaf has %s commits; want its first and the template's", count)
	}
	if main, _ := api.Rev(ctx, "main"); main != r.State.Leaves["repos/api"].Checkpoint {
		t.Errorf("main is at %s after the resume, want the template checkpoint", main)
	}
}

// The template is never written through a symlink in the leaf: the write would land outside it.
func TestInitWritesNoTemplateThroughASymlinkInTheLeaf(t *testing.T) {
	r := halfInit(t)
	ctx := context.Background()
	api, _ := r.Leaf("repos/api")
	outside := outsideDir(t)
	if err := os.Symlink(outside, filepath.Join(api.Dir(), "docs")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "symlink") {
		t.Fatalf("Advance() = %+v, %v; want a failure naming the symlink", out, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("the folder outside the leaf holds %d entries; the template was written through the link", len(entries))
	}
}

// A first task that is blocked puts the leaf back to the template, not to a main that holds only the
// first commit (final review I-2): the next task in the leaf still has its go.mod or package.json.
func TestABlockedFirstTaskKeepsTheTemplateOfANewLeaf(t *testing.T) {
	r := initRun(t, galleryLeaves)
	ctx := context.Background()
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	api, _ := r.Leaf("repos/api")
	main, _ := api.Rev(ctx, "main")
	rec := r.State.Leaves["repos/api"]
	rec.TaskBase = main // what a task records when it starts
	r.State.Leaves["repos/api"] = rec
	if err := blockTask(ctx, r, nil, schema.Node{Repo: "repos/api", Task: "gallery-T1"}, "gave up."); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(api.Dir(), "AGENTS.md")); err != nil || string(b) != "rules of helheim-go\n" {
		t.Errorf("AGENTS.md = %q, %v; the blocked task's reset deleted the template", b, err)
	}
	if now, _ := api.Rev(ctx, "main"); now != main {
		t.Errorf("main moved to %s from %s", now, main)
	}
}

// recordedAt is the commit the project's umbrella records for the leaf at rel in its HEAD: the commit the
// acceptance snapshot takes the leaf at (B-048 M-1).
func recordedAt(t *testing.T, r *run.Run, rel string) string {
	t.Helper()
	got, err := git.Run(context.Background(), r.Project, "rev-parse", "HEAD:"+rel)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A leaf init creates is recorded in the umbrella at its template, not at its first commit: the first
// pointer move's acceptance snapshot reads every other leaf at the recorded commit, so a new leaf recorded
// at its first commit would be seen without its template (B-061).
func TestInitRecordsEachNewLeafInTheUmbrellaAtItsTemplate(t *testing.T) {
	for name, leaves := range map[string]string{
		"paths sort in the order the leaves are made": galleryLeaves,
		"the consumer's path sorts first":             strings.NewReplacer("repos/api", "repos/z-api", "repos/web", "repos/a-web").Replace(galleryLeaves),
	} {
		t.Run(name, func(t *testing.T) {
			r := initRun(t, leaves)
			ctx := context.Background()
			if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
				t.Fatalf("Advance() = %+v, %v", out, err)
			}
			u, _ := schema.LoadUmbrella(r.Project)
			for _, p := range u.Paths() {
				l, _ := r.Leaf(p)
				main, _ := l.Rev(ctx, "main")
				if got := recordedAt(t, r, p); got != main || got != r.State.Leaves[p].Checkpoint {
					t.Errorf("%s: the umbrella records %s, want the template at %s", p, got, main)
				}
			}
			if st, _ := git.Run(ctx, r.Project, "status", "--porcelain", "--untracked-files=no"); st != "" {
				t.Errorf("init leaves the umbrella dirty: %q", st)
			}
		})
	}
}

// An init whose record of a template fails, here on staged changes Odin didn't make in the umbrella, after
// main moved to the template, finishes the record when run again: the umbrella ends at the template of every
// leaf, not at its first commit (B-061).
func TestAnInitWhoseRecordFailedRecordsTheTemplateWhenRunAgain(t *testing.T) {
	r := initRun(t, galleryLeaves)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(r.Project, "notes.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, r.Project, "add", "--", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(ctx, r)
	if err == nil && (out.Status == run.StatusDone || out.Err == nil || !strings.Contains(out.Err.Error(), "staged changes")) {
		t.Fatalf("Advance() = %+v, %v; want the record refused on the staged file", out, err)
	}
	if _, err := git.Run(ctx, r.Project, "reset", "-q", "--", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() the second time = %+v, %v", out, err)
	}
	for _, p := range []string{"repos/api", "repos/web"} {
		l, _ := r.Leaf(p)
		if main, _ := l.Rev(ctx, "main"); recordedAt(t, r, p) != main || main != r.State.Leaves[p].Checkpoint {
			t.Errorf("%s: the umbrella records %s, want the template at %s", p, recordedAt(t, r, p), main)
		}
	}
}

// Prepare refuses an unpinned leaf the project has when the run starts, so init's own check guards a leaf
// that joins the project after that. The test adds one to the run's view of the project, and runs init.
func TestInitRefusesAnExistingLeafThatPinsNoRealmUnderAGraphPackWithNoRecipe(t *testing.T) {
	graph := realmFiles{name: "graph-toy", version: "0.1.0"}
	r := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n",
		realms: []realmFiles{graph, recipeRealm("helheim-go", "0.2.0", "api.txt", "API.")}})
	if err := os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	leaves := "leaves:\n  repos/api:\n    realm: helheim-go\n    realm_version: 0.2.0\n    description: The API.\n  repos/web:\n    realm: helheim-go\n    realm_version: 0.2.0\n    description: The web app.\n"
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(leaves), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Setup.Project.Umbrella.Children["repos/api"] = schema.Child{Kind: schema.ChildLeaf}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the leaf repos/api pins no realm") {
		t.Fatalf("Advance() = %+v, %v; want a refusal naming the leaf", out, err)
	}
	if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 0 {
		t.Errorf("init created %v before it refused", u.Paths())
	}
}

func TestInitIgnoresTheFactoryFolderOfTheProject(t *testing.T) {
	ctx := context.Background()
	for name, before := range map[string]string{"no .gitignore": "", "an existing one": "node_modules/\n", "one with the line": "node_modules/\n.factory/\n", "one with no final newline": "node_modules/"} {
		t.Run(name, func(t *testing.T) {
			r := initRun(t, galleryLeaves)
			path := filepath.Join(r.Project, ".gitignore")
			if before != "" {
				if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"add", "--", ".gitignore"}, {"commit", "-q", "-m", "a .gitignore", "--", ".gitignore"}} {
					if _, err := git.Run(ctx, r.Project, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
				t.Fatalf("Advance() = %+v, %v", out, err)
			}
			if _, err := git.Run(ctx, r.Project, "check-ignore", "-q", ".factory/runs"); err != nil {
				t.Errorf("git check-ignore .factory/runs = %v; the project's .gitignore must name .factory/", err)
			}
			b, _ := os.ReadFile(path)
			if n := strings.Count(string(b), ".factory/\n"); n != 1 || !strings.HasPrefix(string(b), strings.TrimSuffix(before, "\n")) {
				t.Errorf(".gitignore = %q after %q; want the old lines kept and .factory/ once", b, before)
			}
			if st, _ := git.Run(ctx, r.Project, "status", "--porcelain", "--", ".gitignore"); st != "" {
				t.Errorf(".gitignore is left uncommitted: %q", st)
			}
		})
	}
}
