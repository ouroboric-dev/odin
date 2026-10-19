package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// graphOnlyStore is a graph store that holds one graph pack, softy, with a graph and no recipe: no
// checks, no leaf, no sandbox, no services. Its one node works in repos/api.
func graphOnlyStore(t *testing.T) (cfg string) {
	t.Helper()
	store := t.TempDir()
	dir := filepath.Join(store, "softy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{
		"pack.yaml":  "name: softy\nversion: 0.1.0\ngraph: graph.yaml\n",
		"graph.yaml": "name: softy\nversion: 0.1.0\nschema: 1\nnodes:\n  - { id: look, kind: exec, run: [echo, hi], repo: repos/api }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"graphs:\n  dir: "+store+"\n")
}

func TestALeafWithNoPinStopsTheRunBeforeAnythingStarts(t *testing.T) {
	project := treetest.Build(t, pinned("softy", "0.1.0", map[string]treetest.Pin{"repos/api": {}}))
	_, err := Start(ctx, Options{Project: project, Config: graphOnlyStore(t), Graph: "softy"})
	if err == nil || !strings.Contains(err.Error(), "the leaf repos/api pins no realm") || !strings.Contains(err.Error(), "realm and realm_version in umbrella.yaml") {
		t.Fatalf("Start() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".factory", "runs")); err == nil {
		t.Error("a run folder was made for a project that was refused")
	}
}

func TestAPackThatHoldsAGraphAndARecipeServesTheLeavesThatPinNone(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: toy(t)})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	defer r.Close()
	rl, err := r.Setup.RealmOf("repos/app")
	if err != nil || rl.Name != "toy" {
		t.Fatalf("RealmOf(repos/app) = %v, %v; want toy, which holds the graph and the recipe", rl, err)
	}
	merge := r.Setup.Graph.Nodes[0]
	for _, n := range r.Setup.Graph.Nodes {
		if n.ID == "merge" {
			merge = n
		}
	}
	nr, err := r.Setup.NodeRealm(merge)
	if err != nil || nr.Name != "toy" || len(nr.Pack.Checks[merge.Checks[0]]) == 0 {
		t.Errorf("NodeRealm(merge) = %v, %v; its check %s comes from toy", nr, err, merge.Checks)
	}
	if _, ok := r.State.Realms["toy"]; !ok {
		t.Errorf("run.json realms = %v; want toy", r.State.Realms)
	}
}
