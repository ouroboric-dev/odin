package run

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// startToyRun starts a toy run in a fresh project and returns it, open.
func startToyRun(t *testing.T, config string) *Run {
	t.Helper()
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), config), Realm: toy(t)})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	return r
}

func TestARunWorksInItsProjectsLeaves(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	id := r.State.ID
	r.Close()
	nodes, err := tree.Resolve(ctx, project, "repos/app")
	if err != nil {
		t.Fatal(err)
	}
	app := git.Repo{GitDir: nodes[1].GitDir, WorkTree: nodes[1].Dir}
	s, err := LoadState(Dir(project, id))
	if err != nil {
		t.Fatal(err)
	}
	rec := s.Leaves["repos/app"]
	head, _ := app.Run(ctx, "rev-parse", "HEAD")
	branch, _ := app.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if rec.Branch != "ygg/"+id || branch != rec.Branch || rec.Base != head || rec.Checkpoint != head {
		t.Errorf("leaf record = %+v; the leaf is on %s at %s", rec, branch, head)
	}
	if s.Root != project || s.ProjectPath != "" {
		t.Errorf("run.json places the run at %s in %s", s.ProjectPath, s.Root)
	}
}

func TestStartRefusesAProjectOutsideTheTree(t *testing.T) {
	outside := t.TempDir()
	_, err := Start(ctx, Options{Project: outside, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err == nil || !strings.Contains(err.Error(), "not inside an instance tree") {
		t.Errorf("Start() = %v, want a refusal: a resume resets leaves, so Odin works only in the tree", err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".factory")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the refused Start created .factory/")
	}
	r := startToyRun(t, scriptedRoles+fullCaps)
	r.Close()
	leafDir := r.RepoDir("repos/app")
	if _, err := Start(ctx, Options{Project: leafDir, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)}); err == nil || !strings.Contains(err.Error(), "is a leaf") {
		t.Errorf("Start() in a leaf = %v, want a refusal", err)
	}
}

func TestPrepareRefusesAGraphTheProjectCantRun(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cases := map[string]struct{ nodes, want string }{
		"a node with no repo":       {"  - { id: a, kind: exec, run: [echo] }\n", "names no repo"},
		"an agent in the umbrella":  {"  - { id: a, kind: agent, role: planner, prompt: p.md, repo: . }\n", "is an umbrella"},
		"a repo nobody creates":     {"  - { id: a, kind: exec, run: [echo], repo: repos/other }\n", "the realm doesn't create"},
		"a check the pack lacks":    {"  - { id: m, kind: merge, repo: repos/app, checks: [lint] }\n", "doesn't define"},
		"a pointer for an umbrella": {"  - { id: p, kind: pointer, repo: repos/app }\n  - { id: q, kind: pointer, repo: sub }\n", "the project lacks"},
	}
	for name, c := range cases {
		realm := t.TempDir()
		files := map[string]string{
			"pack.yaml":                     "name: t\nversion: 0.1.0\ngraph: graph.yaml\nchecks:\n  unit: [echo]\nleaves:\n  repos/app: { description: d, template: tpl, first_commit: [LICENSE] }\n",
			"graph.yaml":                    "name: t\nversion: 1.0.0\nnodes:\n" + c.nodes,
			"p.md":                          "Do it.\n",
			filepath.Join("tpl", "LICENSE"): "AGPL-3.0-only\n",
		}
		for f, body := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(realm, f)), 0o755)
			if err := os.WriteFile(filepath.Join(realm, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := Prepare(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: realm})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Prepare() = %v, want an error containing %q", name, err, c.want)
		}
	}
}

func TestAFailureAfterTheRunStartedIsRecorded(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	nodes, err := tree.Resolve(ctx, project, "repos/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodes[1].Dir, "README.md"), []byte("a person's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	var f *Failed
	if !errors.As(err, &f) || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("Start() = %v, want a *Failed: the leaf has a person's uncommitted edit", err)
	}
	s, err := LoadState(Dir(project, f.ID))
	if err != nil || s.Status != StatusFailed || !strings.Contains(s.Error, "uncommitted") {
		t.Errorf("run.json = %+v, %v; want the run recorded as failed, with why", s, err)
	}
}

func TestTheRealmCreatesTheLeavesAProjectLacks(t *testing.T) {
	project := treetest.Project(t) // no leaves yet
	commits, _ := git.Run(ctx, project, "rev-list", "--count", "HEAD")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	r.Close()
	nodes, err := tree.Resolve(ctx, project, "repos/app")
	if err != nil {
		t.Fatalf("the toy realm's leaf isn't in the tree: %v", err)
	}
	app := git.Repo{GitDir: nodes[1].GitDir, WorkTree: nodes[1].Dir}
	if files, _ := app.Run(ctx, "ls-tree", "-r", "--name-only", r.State.Leaves["repos/app"].Base); files != "LICENSE" {
		t.Errorf("the leaf's first commit holds %q, want the template's LICENSE only", files)
	}
	if after, _ := git.Run(ctx, project, "rev-list", "--count", "HEAD"); after == commits {
		t.Error("the project has no commit recording the new leaf")
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "ledger.jsonl")); !strings.Contains(string(b), `"event":"repo_created"`) {
		t.Error("the ledger doesn't record the created leaf")
	}
	second, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil {
		t.Fatalf("a second run in the project = %v", err)
	}
	second.Close()
	if b, _ := os.ReadFile(filepath.Join(second.Dir, "ledger.jsonl")); strings.Contains(string(b), "repo_created") {
		t.Error("the second run created the leaf again")
	}
}

func TestOneRunAdvancesAProjectAtATime(t *testing.T) {
	r := startToyRun(t, scriptedRoles+fullCaps)
	defer r.Close()
	_, err := Start(ctx, Options{Project: r.Project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if !errors.Is(err, ErrProjectBusy) {
		t.Errorf("a second run in a busy project = %v, want ErrProjectBusy", err)
	}
	entries, _ := os.ReadDir(filepath.Join(r.Project, ".factory", "runs"))
	if len(entries) != 1 {
		t.Errorf("the refused run left a directory: %d runs", len(entries))
	}
}

func TestOpenPutsLeavesBackAtTheirCheckpoints(t *testing.T) {
	r := startToyRun(t, scriptedRoles+fullCaps)
	id, project, app := r.State.ID, r.Project, r.RepoDir("repos/app")
	r.Close()
	if err := os.WriteFile(filepath.Join(app, "half-done.txt"), []byte("an interrupted node's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, project, id, "") // the run is "running": it was interrupted, so its node restarts
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(app, "half-done.txt")); err == nil {
		t.Error("Open() kept an interrupted node's work; the node restarts from its checkpoint")
	}
	r.State.Status, r.State.Position, r.State.Pending, r.State.PendingGate = StatusStopped, 1, "design-gate", gungnir.GateDesign
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := os.WriteFile(filepath.Join(app, "LICENSE"), []byte("changed by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, project, id, ""); err == nil || !strings.Contains(err.Error(), "changed while the run waited") {
		t.Errorf("Open() of a waiting run with a changed leaf = %v, want a refusal", err)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "LICENSE")); string(b) != "changed by hand\n" {
		t.Error("Open() discarded a person's change")
	}
}

func TestAStricterModeSwitchIsAllowed(t *testing.T) {
	r := startToyRun(t, "mode: unattended\ngates: { design: auto }\n"+scriptedRoles+fullCaps)
	id, project := r.State.ID, r.Project
	r.Close()
	r, err := Open(ctx, project, id, gungnir.ModeStepByStep)
	if err != nil {
		t.Fatalf("Open() with a stricter mode = %v, want it allowed: design: auto applies only in unattended mode", err)
	}
	defer r.Close()
	if r.State.Mode != gungnir.ModeStepByStep {
		t.Errorf("mode = %s", r.State.Mode)
	}
}

func TestRolesFollowAConfigChange(t *testing.T) {
	cfgDir := t.TempDir()
	project := treetest.Project(t)
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, cfgDir, scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close()
	writeConfig(t, cfgDir, strings.ReplaceAll(scriptedRoles, "model: none", "model: other")+fullCaps)
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.State.Roles["planner"].Model; got != "other" {
		t.Errorf("run.json records the planner's model as %q after the config changed, want other", got)
	}
}

// A project MAY hold sub-projects (change project-is-its-own-tree): a run in one finds the root
// above it, and works in the sub-project's own leaves.
func TestARunWorksInASubProject(t *testing.T) {
	root, sub := treetest.SubProject(t, "repos/app")
	r, err := Start(ctx, Options{Project: sub, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil {
		t.Fatalf("Start() in a sub-project = %v", err)
	}
	defer r.Close()
	if r.State.Root != root || r.State.ProjectPath != "projects/sub" {
		t.Errorf("run.json places the run at %q in %s, want projects/sub in %s", r.State.ProjectPath, r.State.Root, root)
	}
	if got := r.RepoDir("repos/app"); got != filepath.Join(sub, "repos", "app") {
		t.Errorf("the run works in %s, want the sub-project's own leaf", got)
	}
}
