package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// The exit criterion of the realm work, with scripted agents: a run with --graph midgard-toy walks that
// realm's graph; the spec stage names the leaves and their realms; init creates the API from
// helheim-toy's template and the web leaf from midgard-toy's; each task runs in its leaf with the
// recipe of its leaf's realm; and run.json and the status line record every realm and its version.

var ctx = context.Background()

func realmsDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/realms")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// galleryConfig writes a factory.yaml for the toy realms with the given mode and script file.
func galleryConfig(t *testing.T, mode, gates, script string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	body := "mode: " + mode + "\n" + gates + "roles:\n  planner: { agent: scripted, model: none, script: " + abs + " }\n  implementer: { agent: scripted, model: none, script: " + abs + " }\n" +
		"budget:\n  per_run: { tokens: 1000000, turns: 1000 }\n  per_day: { tokens: 2000000, turns: 2000 }\nrealms:\n  dir: " + realmsDir(t) + "\ngraphs:\n  dir: " + realmsDir(t) + "\n"
	p := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestARunOfTheToyRealmsBuildsAProductFromTwoRealms(t *testing.T) {
	dir := treetest.Project(t)
	cfg := galleryConfig(t, "key-points", "", "../../testdata/realms-scripts/gallery.yaml")
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "midgard-toy")
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point design-gate") {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	id := runID(t, dir)
	code, out, errOut = ygg(t, "approve", id, "design-gate", "--project", dir)
	if code != ExitDone || !strings.Contains(out, "is done.") {
		t.Fatalf("approve: exit %d:\n%s%s", code, out, errOut)
	}

	// The documents of the three stages are in the project, committed by Odin.
	for _, f := range []string{"docs/1-product/design.md", "docs/2-engineering/specs/spec.md", "docs/leaves.yaml", "docs/2-engineering/plans/2026-10-17-gallery/tasks.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s was not published: %v", f, err)
		}
	}
	// The plan with a mistake went back to the planner once.
	st, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if st.Returns["ready"] != 1 || st.Attempts["plan"] != 2 || st.Status != run.StatusDone {
		t.Errorf("returns %v, plan attempts %d, status %s", st.Returns, st.Attempts["plan"], st.Status)
	}
	// The project pins the graph and each leaf's realm, and the leaves were made from their templates.
	u, err := schema.LoadUmbrella(dir)
	if err != nil {
		t.Fatal(err)
	}
	api, web := u.Children["repos/api"], u.Children["repos/web"]
	if u.Graph != "midgard-toy" || api.Realm != "helheim-toy" || web.Realm != "midgard-toy" || web.Consumes[0] != "repos/api" {
		t.Errorf("the project's pins: graph %s; api %+v; web %+v", u.Graph, api, web)
	}
	for leaf, want := range map[string]string{"repos/api": "# AGENTS.md: the toy API leaf\n", "repos/web": "# AGENTS.md: the toy web leaf\n"} {
		if b, err := os.ReadFile(filepath.Join(dir, leaf, "AGENTS.md")); err != nil || string(b) != want {
			t.Errorf("%s/AGENTS.md = %q, %v; the leaf is made from the template of its own realm", leaf, b, err)
		}
	}
	// Each task landed in its leaf, and the project points at what landed.
	for leaf, files := range map[string][]string{"repos/api": {"api.txt", "api-own.txt"}, "repos/web": {"web.txt", "photos.txt"}} {
		for _, f := range files {
			if body, err := git.Run(ctx, filepath.Join(dir, leaf), "show", "main:"+f); err != nil || body != "done" {
				t.Errorf("%s: main:%s = %q, %v", leaf, f, body, err)
			}
		}
		main, _ := git.Run(ctx, filepath.Join(dir, leaf), "rev-parse", "main")
		if got, _ := git.Run(ctx, dir, "rev-parse", "HEAD:"+leaf); got != main {
			t.Errorf("the project points at %s for %s, want main %s", got, leaf, main)
		}
	}
	feats, err := schema.LoadFeatures(filepath.Join(dir, ".factory", "features.json"))
	if err != nil || len(feats.Tasks) != 4 {
		t.Fatalf("features.json = %+v, %v", feats, err)
	}
	for _, f := range feats.Tasks {
		if f.Status != schema.TaskDone {
			t.Errorf("task %s is %s", f.ID, f.Status)
		}
	}
	// run.json and the status line record the lead realm with its graph, and each leaf's realm.
	if st.Graph != "midgard-toy" || st.GraphVersion != "0.1.0" || len(st.Realms) != 2 || st.Leaves["repos/api"].Realm != "helheim-toy" || st.Leaves["repos/web"].Realm != "midgard-toy" {
		t.Errorf("run.json: graph %s %s, realms %v, leaves %+v", st.Graph, st.GraphVersion, st.Realms, st.Leaves)
	}
	_, status, _ := ygg(t, "status", id, "--project", dir)
	for _, want := range []string{"graph:    midgard-toy 0.1.0", "(helheim-toy 0.1.0)", "(midgard-toy 0.1.0)", "tasks:    4 (4 done, 0 doing, 0 to do, 0 blocked)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	// Doctor is content with the finished run. The gallery's realms store holds realm packs with no graph, which a
	// graphs store may not, so doctor reads a config whose graphs are elsewhere.
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	doctorCfg := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(doctorCfg, bytes.Replace(b, []byte("graphs:\n  dir: "+realmsDir(t)), []byte("graphs:\n  dir: "+graphStore(t)), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", doctorCfg); code != ExitDone || !strings.Contains(out, "OK    realms: "+realmsDir(t)) {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
}

func TestAnUnattendedRunOfTheToyRealmsGivesUpATaskAndExitsFive(t *testing.T) {
	dir := treetest.Project(t)
	script, err := os.ReadFile("../../testdata/realms-scripts/gallery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Task 2's implementer builds nothing, twice, so its loop gives up; task 4 depends on it.
	broken := strings.Replace(string(script), "    - actions: [ { write: { path: web.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: built the page, usage: { in: 2000, out: 200, turns: 4 } }\n",
		"    - result: { status: ok, summary: did nothing }\n    - result: { status: ok, summary: did nothing again }\n", 1)
	if broken == string(script) {
		t.Fatal("the script was not changed")
	}
	path := filepath.Join(t.TempDir(), "gallery-broken.yaml")
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := galleryConfig(t, "unattended", "gates:\n  design: auto\n", path)
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "midgard-toy")
	if code != ExitBlocked {
		t.Fatalf("exit %d, want %d:\n%s%s", code, ExitBlocked, out, errOut)
	}
	for _, want := range []string{"is done, with 2 task(s) given up", "gallery-T2:", "gallery-T4: it depends on gallery-T2, which is blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	feats, err := schema.LoadFeatures(filepath.Join(dir, ".factory", "features.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]schema.TaskStatus{}
	for _, f := range feats.Tasks {
		got[f.ID] = f.Status
	}
	if got["gallery-T1"] != schema.TaskDone || got["gallery-T3"] != schema.TaskDone || got["gallery-T2"] != schema.TaskBlocked || got["gallery-T4"] != schema.TaskBlocked {
		t.Errorf("task statuses = %v; the API tasks go on, and the web tasks are given up", got)
	}
	_, status, _ := ygg(t, "status", runID(t, dir), "--project", dir)
	if !strings.Contains(status, "blocked:  gallery-T2:") || !strings.Contains(status, "tasks:    4 (2 done, 0 doing, 0 to do, 2 blocked)") {
		t.Errorf("status:\n%s", status)
	}
}

func TestARunWhoseRealmPinsDisagreeIsRefusedAndCreatesNothing(t *testing.T) {
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-toy", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-toy", Version: "0.9.0"}}}}
	dir := treetest.Build(t, root)
	cfg := galleryConfig(t, "key-points", "", "../../testdata/realms-scripts/gallery.yaml")
	code, _, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "midgard-toy")
	if code != ExitRefused || !strings.Contains(errOut, "the realm store holds helheim-toy 0.1.0, but repos/api pins 0.9.0") {
		t.Errorf("exit %d:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory")); err == nil {
		t.Error("a refused run created .factory/")
	}
	// A realm named with no store to find it in says where to set one.
	plain := galleryConfig(t, "key-points", "", "../../testdata/realms-scripts/gallery.yaml")
	b, _ := os.ReadFile(plain)
	noStore := filepath.Join(t.TempDir(), "factory.yaml")
	os.WriteFile(noStore, []byte(strings.Split(string(b), "realms:")[0]), 0o644)
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", noStore, "--graph", "midgard-toy"); code != ExitRefused || !strings.Contains(errOut, "graphs") {
		t.Errorf("a name with no store: exit %d:\n%s", code, errOut)
	}
}
