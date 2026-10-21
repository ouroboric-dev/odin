package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// realmStore copies the toy realm into a store once for each name, at the given version, and returns the
// store and a factory.yaml that names it.
func realmStore(t *testing.T, versions map[string]string) (store, cfg string) {
	t.Helper()
	store = t.TempDir()
	for name, v := range versions {
		dir := filepath.Join(store, name)
		if err := os.CopyFS(dir, os.DirFS(toy(t))); err != nil {
			t.Fatal(err)
		}
		pack, _ := os.ReadFile(filepath.Join(dir, "pack.yaml"))
		pack = []byte(strings.Replace(strings.Replace(string(pack), "name: toy", "name: "+name, 1), "version: 0.3.0", "version: "+v, 1))
		if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), pack, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store, writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n")
}

// pinned is a project that pins the graph graph at version, and each leaf's realm.
func pinned(graph, version string, leaves map[string]treetest.Pin) treetest.Node {
	n := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: graph, Version: version}, Children: map[string]treetest.Node{}}
	for path, p := range leaves {
		n.Children[path] = treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: p}
	}
	return n
}

func TestARealmIsNamedAndFoundInTheStore(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0"})
	project := treetest.Project(t, "repos/app")
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if s.GraphPack.Name != "midgard-react" || s.Graph == nil || len(s.Realms.Realms) != 1 {
		t.Errorf("graph pack = %+v, realms = %v", s.GraphPack, s.Realms.Names())
	}
	if rl, err := s.RealmOf("repos/app"); err != nil || rl != s.GraphPack {
		t.Errorf("RealmOf(repos/app) = %v, %v; a leaf with no pin is served by a graph pack that holds a recipe", rl, err)
	}
}

func TestAGraphNameWithNoStoreIsRefusedWithAHint(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "graphs section") {
		t.Errorf("Prepare() = %v", err)
	}
}

// A realm pack is a leaf's flavor: --graph refuses it, by name and by path, however much it holds.
func TestARealmPackIsRefusedAsAGraph(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"helheim-go": "0.1.0"})
	pack := filepath.Join(store, "helheim-go", "pack.yaml")
	b, _ := os.ReadFile(pack)
	os.WriteFile(pack, []byte(strings.Replace(string(b), "graph: graph.yaml\n", "", 1)), 0o644)
	realmPath, err := filepath.Abs("../../testdata/realms/helheim-toy")
	if err != nil {
		t.Fatal(err)
	}
	for name, graph := range map[string]string{"by name": "helheim-go", "by path (helheim-toy)": realmPath} {
		if _, err := Prepare(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: cfg, Graph: graph}); err == nil || !strings.Contains(err.Error(), "holds no graph") {
			t.Errorf("%s: Prepare() = %v", name, err)
		}
	}
}

func TestEachLeafUsesTheRealmTheProjectPinsForIt(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/api": {Realm: "helheim-go", Version: "0.2.0"}, "repos/old": {}}))
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if got := strings.Join(s.Realms.Names(), ","); got != "helheim-go,midgard-react" {
		t.Errorf("realms = %s", got)
	}
	realmOf := func(repo string) *realm.Realm {
		t.Helper()
		rl, err := s.RealmOf(repo)
		if err != nil {
			t.Fatalf("RealmOf(%s) = %v", repo, err)
		}
		return rl
	}
	api, app, old := realmOf("repos/api"), realmOf("repos/app"), realmOf("repos/old")
	if api.Name != "helheim-go" || app != s.GraphPack || old != s.GraphPack {
		t.Errorf("RealmOf: api %s, app %s, old %s", api.Name, app.Name, old.Name)
	}
}

func TestPinsThatDisagreeStopTheRunBeforeAnythingIsWritten(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.1.0"})
	disagree := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/web": {Realm: "midgard-react", Version: "0.2.0"}, "repos/api": {Realm: "helheim-go", Version: "0.1.0"}}))
	_, err := Start(ctx, Options{Project: disagree, Config: cfg, Graph: "midgard-react"})
	if err == nil || !strings.Contains(err.Error(), "midgard-react is pinned at 0.1.0 by repos/app and at 0.2.0 by repos/web") {
		t.Errorf("Start() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(disagree, ".factory")); err == nil {
		t.Error("a refused start created .factory/")
	}
	other := treetest.Build(t, pinned("midgard-react", "0.1.0", nil))
	if _, err := Prepare(ctx, Options{Project: other, Config: cfg, Graph: "helheim-go"}); err == nil || !strings.Contains(err.Error(), "the project is built with the graph midgard-react 0.1.0") {
		t.Errorf("the graph is another than the project's: %v", err)
	}
	old := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{"repos/web": {Realm: "helheim-go", Version: "0.0.9"}}))
	if _, err := Prepare(ctx, Options{Project: old, Config: cfg, Graph: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "the realm store holds helheim-go 0.1.0, but repos/web pins 0.0.9") {
		t.Errorf("the store holds another version than the pin: %v", err)
	}
}

func TestRunJSONRecordsEveryRealmAndAResumeRefusesOneThatChanged(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/api": {Realm: "helheim-go", Version: "0.2.0"}}))
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	id := r.State.ID
	got := r.State.Realms
	r.Close()
	if got["helheim-go"].Version != "0.2.0" || got["helheim-go"].Dir != filepath.Join(store, "helheim-go") || len(got["midgard-react"].SHA256) != 64 || len(got) != 2 {
		t.Errorf("Realms = %+v", got)
	}
	if s, _ := LoadState(Dir(project, id)); s.Graph != "midgard-react" || s.GraphVersion != "0.1.0" || len(s.Realms) != 2 {
		t.Errorf("run.json realms = %+v", s.Realms)
	}
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() with the realms as they were = %v", err)
	}
	r.Close()
	prompt := filepath.Join(store, "helheim-go", "prompts")
	entries, _ := os.ReadDir(prompt)
	changed := filepath.Join(prompt, entries[0].Name())
	original, err := os.ReadFile(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("changed under the run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, project, id, "")
	if err == nil || !strings.Contains(err.Error(), "the realm helheim-go changed since the run started") {
		t.Fatalf("Open() after a prompt of a leaf's realm changed = %v", err)
	}
	// The refusal says what to do: it names the folder, shows the hash the run began with and the one the
	// realm has now, and says that putting the realm back lets the run resume or be graded (B-083).
	was := got["helheim-go"].SHA256[:12]
	for _, want := range []string{filepath.Join(store, "helheim-go"), "it was " + was, "put the realm back", "grade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if err := os.WriteFile(changed, original, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() with the realm put back = %v", err)
	}
	r.Close()
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheFirstRunPinsTheGraphItWasGivenByName(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0"})
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	r.Close()
	u, err := schema.LoadUmbrella(project)
	if err != nil || u.Graph != "midgard-react" || u.GraphVersion != "0.1.0" {
		t.Fatalf("umbrella.yaml = %+v, %v; want the graph pinned", u, err)
	}
	raw, _ := os.ReadFile(filepath.Join(project, "umbrella.yaml"))
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "realm:") || strings.HasPrefix(line, "realm_version:") {
			t.Errorf("umbrella.yaml has a top-level %q; the project pins a graph, not a realm", line)
		}
	}
	if !strings.Contains(string(raw), "graph: midgard-react") || !strings.Contains(string(raw), "graph_version: 0.1.0") {
		t.Errorf("umbrella.yaml lacks graph and graph_version:\n%s", raw)
	}
	if msg := gitOut(t, project, "log", "-1", "--format=%s", "--", "umbrella.yaml"); msg != "Pin the graph midgard-react 0.1.0" {
		t.Errorf("the pin's commit says %q", msg)
	}
	if st := gitOut(t, project, "status", "--porcelain", "--", "umbrella.yaml"); st != "" {
		t.Errorf("umbrella.yaml is left uncommitted: %q", st)
	}
	ledger, _ := os.ReadFile(filepath.Join(Dir(project, r.State.ID), "ledger.jsonl"))
	first, _, _ := strings.Cut(string(ledger), "\n")
	if !strings.Contains(first, `"event":"run_started"`) || !strings.Contains(first, "graph: midgard-react 0.1.0 sha256:") || !strings.Contains(first, "realms: midgard-react 0.1.0 sha256:") ||
		strings.Contains(first, "ygg.realm") {
		t.Errorf("the first ledger line lacks the realms:\n%s", first)
	}
	// The next run finds the pin and agrees with it.
	r2, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("a second Start() = %v", err)
	}
	r2.Close()
	if n := gitOut(t, project, "rev-list", "--count", "HEAD", "--", "umbrella.yaml"); n != "2" {
		t.Errorf("umbrella.yaml has %s commits; the second run pinned nothing", n)
	}
	// A pack given by path pins nothing.
	byPath := treetest.Project(t, "repos/app")
	before := gitOut(t, byPath, "rev-parse", "HEAD")
	r3, err := Start(ctx, Options{Project: byPath, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	r3.Close()
	if gitOut(t, byPath, "rev-parse", "HEAD") != before {
		t.Error("a run with a pack given by path committed to the project")
	}
}

// sandboxedPack makes a pack with a sandbox section, as name at version, in a new folder of the store dir.
func sandboxedPack(t *testing.T, store, name, version string) string {
	t.Helper()
	sandboxRealm(t, store, name, version, nodeStack, "proxy.golang.org", "")
	return filepath.Join(store, name)
}

func TestARunWhoseGraphIsNotTheProjectsPinStopsBeforeAnythingStarts(t *testing.T) {
	// The packs have a sandbox section, so a run that starts opens a sandbox and the fake counts it.
	store := t.TempDir()
	sandboxedPack(t, store, "midgard-react", "0.2.0")
	sandboxedPack(t, store, "midgard-vue", "0.1.0")
	byPath := func(name, version string) string { return sandboxedPack(t, t.TempDir(), name, version) }
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n")
	cases := map[string]struct {
		realm string
		want  bool // the run starts
	}{
		"another name, by name":                {"midgard-vue", false},
		"the same name, another version, name": {"midgard-react", false},
		"the same name, another version, path": {byPath("midgard-react", "0.2.0"), false},
		"the same name and version, by path":   {byPath("midgard-react", "0.1.0"), true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{"repos/app": {}, "repos/api": {}}))
			started := 0
			h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { started++; return &sandboxfake.Sandbox{}, nil }}
			r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: c.realm, Hooks: h})
			if c.want {
				if err != nil {
					t.Fatalf("Start() = %v; the same graph at the same version is the project's", err)
				}
				r.Close()
				if started == 0 {
					t.Error("the run started and no sandbox was opened: the counter is not live")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "the project is built with the graph") {
				t.Fatalf("Start() = %v, want a refusal that names the project's graph", err)
			}
			if _, err := os.Stat(filepath.Join(project, ".factory")); err == nil {
				t.Error("a refused start created .factory/")
			}
			if started != 0 {
				t.Errorf("%d sandboxes started", started)
			}
		})
	}
}

func TestARealmGivenByPathIsNotPinned(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	before := gitOut(t, project, "rev-parse", "HEAD")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if gitOut(t, project, "rev-parse", "HEAD") != before {
		t.Error("a run with a realm given by path committed to the project: it pins nothing")
	}
}

func TestARunWontPinOverUncommittedWork(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0"})
	project := treetest.Project(t, "repos/app")
	f := filepath.Join(project, "umbrella.yaml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte("# the operator's note\n")...), 0o644)
	if _, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "umbrella.yaml") {
		t.Errorf("Start() = %v; the operator's own edit of umbrella.yaml is not Odin's to commit", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".factory", "runs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(project, ".factory", "runs"))
		if len(entries) != 0 {
			t.Errorf("a refused start left run directories: %v", entries)
		}
	}
}

func TestARunTakesTheRealmsNewPinsNameAfterItStarted(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", nil))
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.AddRealms([]schema.Pin{{Realm: "helheim-go", Version: "0.2.0", Where: "repos/api"}, {Realm: "midgard-react", Version: "0.1.0", Where: "repos/web"}}); err != nil {
		t.Fatalf("AddRealms() = %v", err)
	}
	if got := strings.Join(r.Setup.Realms.Names(), ","); got != "helheim-go,midgard-react" {
		t.Errorf("realms = %s", got)
	}
	if rec := r.State.Realms["helheim-go"]; rec.Version != "0.2.0" || rec.Dir != filepath.Join(store, "helheim-go") || len(rec.SHA256) != 64 {
		t.Errorf("run.json records %+v", r.State.Realms)
	}
	if s, _ := LoadState(r.Dir); len(s.Realms) != 2 {
		t.Errorf("run.json on disk holds %d realms", len(s.Realms))
	}
	ledger, _ := os.ReadFile(filepath.Join(r.Dir, "ledger.jsonl"))
	if strings.Count(string(ledger), `"event":"realm_loaded"`) != 1 || !strings.Contains(string(ledger), `"ygg.realm.name":"helheim-go"`) {
		t.Errorf("the ledger should hold one realm_loaded, for the realm that was new:\n%s", ledger)
	}
	// A pin that disagrees with one the run has changes nothing.
	err = r.AddRealms([]schema.Pin{{Realm: "helheim-go", Version: "0.9.0", Where: "repos/db"}})
	if err == nil || !strings.Contains(err.Error(), "helheim-go is pinned at 0.2.0 by repos/api") && !strings.Contains(err.Error(), "the realm store holds helheim-go 0.2.0, but repos/db pins 0.9.0") {
		t.Errorf("AddRealms() = %v", err)
	}
	if len(r.Setup.Realms.Realms) != 2 || len(r.State.Realms) != 2 {
		t.Error("a refused AddRealms changed the run")
	}
}

// A run that started with no realm records none. A project that names a realm after that is not the project the
// run began with, and the first resume says so: the check does not skip a state whose realms are empty.
func TestAResumeRefusesARealmTheProjectNamesAndTheRunDidNotStartWith(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{"repos/api": {Realm: "helheim-go", Version: "0.2.0"}}))
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.State.Realms = nil // as the state of a run that started before the project named helheim-go
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	_, err = Open(ctx, project, id, "")
	if err == nil || !strings.Contains(err.Error(), "the project now names the realm helheim-go") {
		t.Errorf("Open() = %v, want a refusal that names the realm the run did not start with", err)
	}
}
