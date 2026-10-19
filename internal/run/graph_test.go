package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// graphStore copies the toy pack into <store>/<dir> for each entry, with the pack named packName, and
// returns the store and a factory.yaml whose graphs section names it.
func graphStore(t *testing.T, packs map[string]string) (store, cfg string) {
	t.Helper()
	store = t.TempDir()
	for dir, packName := range packs {
		d := filepath.Join(store, dir)
		if err := os.CopyFS(d, os.DirFS(toy(t))); err != nil {
			t.Fatal(err)
		}
		pack, _ := os.ReadFile(filepath.Join(d, "pack.yaml"))
		pack = []byte(strings.Replace(string(pack), "name: toy", "name: "+packName, 1))
		if err := os.WriteFile(filepath.Join(d, "pack.yaml"), pack, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store, writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"graphs:\n  dir: "+store+"\n")
}

func TestAGraphIsNamedAndFoundInTheGraphStore(t *testing.T) {
	store, cfg := graphStore(t, map[string]string{"software": "software", "wrong": "other"})
	project := treetest.Project(t, "repos/app")
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "software"})
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if s.GraphPack.Name != "software" || s.GraphPack.Dir != filepath.Join(store, "software") || s.Graph == nil {
		t.Errorf("GraphPack = %+v", s.GraphPack)
	}
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "wrong"}); err == nil || !strings.Contains(err.Error(), "holds the graph other") {
		t.Errorf("a folder whose pack is named other: Prepare() = %v", err)
	}
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "missing"}); err == nil || !strings.Contains(err.Error(), "there is no graph missing in "+store) {
		t.Errorf("a name the store lacks: Prepare() = %v", err)
	}
	none := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	if _, err := Prepare(ctx, Options{Project: project, Config: none, Graph: "software"}); err == nil || !strings.Contains(err.Error(), "factory.yaml has no graphs section to find it in") {
		t.Errorf("no graphs section: Prepare() = %v", err)
	}
	s, err = Prepare(ctx, Options{Project: project, Config: none, Graph: filepath.Join(store, "software")})
	if err != nil || s.GraphPack.Name != "software" {
		t.Errorf("a path loads without a store: %v", err)
	}
	// A realm store alone doesn't hold graphs: a name is looked up in graphs.dir only.
	realmsOnly := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"realms:\n  dir: "+store+"\n")
	if _, err := Prepare(ctx, Options{Project: project, Config: realmsOnly, Graph: "software"}); err == nil || !strings.Contains(err.Error(), "factory.yaml has no graphs section to find it in") {
		t.Errorf("a realms section and no graphs section: Prepare() = %v", err)
	}
	// A ./ path is resolved against the working directory, as realm.Resolve does.
	t.Chdir(store)
	s, err = Prepare(ctx, Options{Project: project, Config: none, Graph: "./software"})
	if err != nil {
		t.Fatalf("--graph ./software = %v", err)
	}
	if s.GraphPack.Name != "software" || !filepath.IsAbs(s.GraphPack.Dir) || filepath.Base(s.GraphPack.Dir) != "software" {
		t.Errorf("--graph ./software loaded %q from %q", s.GraphPack.Name, s.GraphPack.Dir)
	}
}

func TestAPackWithNoGraphCannotBeTheGraph(t *testing.T) {
	store, cfg := graphStore(t, map[string]string{"software": "software"})
	pack := filepath.Join(store, "software", "pack.yaml")
	b, _ := os.ReadFile(pack)
	if err := os.WriteFile(pack, []byte(strings.Replace(string(b), "graph: graph.yaml\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: cfg, Graph: "software"})
	if err == nil || !strings.Contains(err.Error(), "the pack software holds no graph; --graph names a graph pack, such as software in Voluspa") {
		t.Errorf("Prepare() = %v", err)
	}
}

func TestRunJSONRecordsTheGraphAndAResumeRefusesOneThatChanged(t *testing.T) {
	store, cfg := graphStore(t, map[string]string{"toy": "toy"})
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "toy"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	id := r.State.ID
	r.Close()
	raw, err := os.ReadFile(filepath.Join(Dir(project, id), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema"] != "ygg.run/v3" || m["graph"] != "toy" || m["graph_version"] != "0.3.0" || m["graph_dir"] != filepath.Join(store, "toy") {
		t.Errorf("run.json = schema %v, graph %v, graph_version %v, graph_dir %v", m["schema"], m["graph"], m["graph_version"], m["graph_dir"])
	}
	if sum, _ := m["graph_pack_sha256"].(string); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
		t.Errorf("graph_pack_sha256 = %q, want 64 hex digits", sum)
	}
	for _, gone := range []string{"realm", "realm_version", "realm_dir"} {
		if _, has := m[gone]; has {
			t.Errorf("run.json still has %q", gone)
		}
	}
	ledger, _ := os.ReadFile(filepath.Join(Dir(project, id), "ledger.jsonl"))
	first, _, _ := strings.Cut(string(ledger), "\n")
	if !strings.Contains(first, "graph: toy 0.3.0 sha256:") || !strings.Contains(first, "realms: ") {
		t.Errorf("the first ledger line lacks the graph note:\n%s", first)
	}
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() with the graph as it was = %v", err)
	}
	r.Close()
	prompt := filepath.Join(store, "toy", "prompts", "build.md")
	if err := os.WriteFile(prompt, []byte("changed under the run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, project, id, ""); err == nil || !strings.Contains(err.Error(), "the graph toy changed since the run started") {
		t.Errorf("Open() after a prompt of the graph pack changed = %v", err)
	}
}

func TestAResumeOfARunLedByARealmIsRefusedAndTouchesNothing(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: toySandbox(t), Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	if _, err := r.Runner(ctx, "repos/app"); err != nil { // a leaf's sandbox comes up, so the counter is live
		t.Fatal(err)
	}
	r.Close()
	// What a run led by a realm left: schema v2, a realm and its realm_dir, and none of the graph fields.
	file := filepath.Join(Dir(project, id), "run.json")
	raw, _ := os.ReadFile(file)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["schema"] = "ygg.run/v2"
	m["realm"], m["realm_version"], m["realm_dir"] = "toy-sandbox", "0.1.0", toySandbox(t)
	for _, k := range []string{"graph", "graph_dir", "graph_pack_sha256"} {
		delete(m, k)
	}
	out, _ := json.Marshal(m)
	if err := os.WriteFile(file, out, 0o644); err != nil {
		t.Fatal(err)
	}
	// The leaf holds work the run didn't make: a commit past its checkpoint and an untracked file. A resume
	// that got as far as resetting the leaf would discard both, so HEAD and the status would move.
	app := filepath.Join(project, "repos/app")
	if err := os.WriteFile(filepath.Join(app, "scratch.txt"), []byte("not the run's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "later.txt"), []byte("committed past the checkpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, app, "add", "--", "later.txt")
	gitOut(t, app, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "past the checkpoint")
	head, status := gitOut(t, app, "rev-parse", "HEAD"), gitOut(t, app, "status", "--porcelain")
	ups, downs := len(fake.Ups), len(fake.Downs)
	_, err = OpenWith(ctx, project, id, "", h)
	if err == nil || !strings.Contains(err.Error(), "can't be resumed") || !strings.Contains(err.Error(), "start a new run with --graph") {
		t.Fatalf("OpenWith() = %v, want the refusal of a run a realm led", err)
	}
	if !strings.Contains(status, "scratch.txt") {
		t.Fatalf("the leaf's status = %q, want the untracked file in it", status)
	}
	if got := gitOut(t, app, "rev-parse", "HEAD"); got != head {
		t.Errorf("the leaf's HEAD moved: %s -> %s", head, got)
	}
	if got := gitOut(t, app, "status", "--porcelain"); got != status {
		t.Errorf("the leaf's status changed: %q -> %q", status, got)
	}
	if len(fake.Ups) != ups || len(fake.Downs) != downs {
		t.Errorf("the refused resume touched the sandbox: %d ups, %d downs, were %d and %d", len(fake.Ups), len(fake.Downs), ups, downs)
	}
	if ups == 0 {
		t.Error("the first run opened no sandbox: the counter is not live")
	}
}
