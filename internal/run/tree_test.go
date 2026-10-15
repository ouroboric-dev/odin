package run

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestARunFindsItsProjectAndReposInTheTree(t *testing.T) {
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
	if r.RepoDir("repos/app") != nodes[1].Dir || r.RepoDir(".") != project {
		t.Errorf("the run's repos are %s and %s", r.RepoDir("repos/app"), r.RepoDir("."))
	}
	s, err := LoadState(Dir(project, id))
	if err != nil {
		t.Fatal(err)
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
		"a repo the project lacks":  {"  - { id: a, kind: exec, run: [echo], repo: repos/other }\n", "the project lacks"},
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
