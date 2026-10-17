package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
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
	return store, writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"realms:\n  dir: "+store+"\n")
}

func pinned(lead, version string, leaves map[string]treetest.Pin) treetest.Node {
	n := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Pin: treetest.Pin{Realm: lead, Version: version}, Children: map[string]treetest.Node{}}
	for path, p := range leaves {
		n.Children[path] = treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: p}
	}
	return n
}

func TestARealmIsNamedAndFoundInTheStore(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0"})
	project := treetest.Project(t, "repos/app")
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"})
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if s.Lead.Name != "midgard-react" || s.Pack != s.Lead.Pack || s.RealmDir != s.Lead.Dir || s.Graph == nil || len(s.Realms.Realms) != 1 {
		t.Errorf("lead = %+v, realms = %v", s.Lead, s.Realms.Names())
	}
	if s.RealmOf("repos/app") != s.Lead || s.Realm("") != s.Lead {
		t.Error("a leaf with no pin uses the lead realm")
	}
}

func TestANameWithNoStoreIsRefusedWithAHint(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "realms") {
		t.Errorf("Prepare() = %v", err)
	}
}

func TestARealmWithoutAGraphCannotLead(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"helheim-go": "0.1.0"})
	pack := filepath.Join(store, "helheim-go", "pack.yaml")
	b, _ := os.ReadFile(pack)
	os.WriteFile(pack, []byte(strings.Replace(string(b), "graph: graph.yaml\n", "", 1)), 0o644)
	if _, err := Prepare(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: cfg, Realm: "helheim-go"}); err == nil || !strings.Contains(err.Error(), "has no graph") {
		t.Errorf("Prepare() = %v", err)
	}
}

func TestEachLeafUsesTheRealmTheProjectPinsForIt(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/api": {Realm: "helheim-go", Version: "0.2.0"}, "repos/old": {}}))
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"})
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if got := strings.Join(s.Realms.Names(), ","); got != "helheim-go,midgard-react" {
		t.Errorf("realms = %s", got)
	}
	if s.RealmOf("repos/api").Name != "helheim-go" || s.RealmOf("repos/app") != s.Lead || s.RealmOf("repos/old") != s.Lead {
		t.Errorf("RealmOf: api %s, app %s, old %s", s.RealmOf("repos/api").Name, s.RealmOf("repos/app").Name, s.RealmOf("repos/old").Name)
	}
}

func TestPinsThatDisagreeStopTheRunBeforeAnythingIsWritten(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.1.0"})
	disagree := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/web": {Realm: "midgard-react", Version: "0.2.0"}, "repos/api": {Realm: "helheim-go", Version: "0.1.0"}}))
	_, err := Start(ctx, Options{Project: disagree, Config: cfg, Realm: "midgard-react"})
	if err == nil || !strings.Contains(err.Error(), "midgard-react is pinned at 0.1.0 by . and at 0.2.0 by repos/web") {
		t.Errorf("Start() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(disagree, ".factory")); err == nil {
		t.Error("a refused start created .factory/")
	}
	other := treetest.Build(t, pinned("midgard-react", "0.1.0", nil))
	if _, err := Prepare(ctx, Options{Project: other, Config: cfg, Realm: "helheim-go"}); err == nil || !strings.Contains(err.Error(), "built with midgard-react, not helheim-go") {
		t.Errorf("the lead realm is another than the project's: %v", err)
	}
	old := treetest.Build(t, pinned("midgard-react", "0.0.9", nil))
	if _, err := Prepare(ctx, Options{Project: old, Config: cfg, Realm: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "the realm store holds midgard-react 0.1.0, but . pins 0.0.9") {
		t.Errorf("the store holds another version than the pin: %v", err)
	}
}

func TestRunJSONRecordsEveryRealmAndAResumeRefusesOneThatChanged(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	project := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/api": {Realm: "helheim-go", Version: "0.2.0"}}))
	r, err := Start(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	id := r.State.ID
	got := r.State.Realms
	r.Close()
	if got["helheim-go"].Version != "0.2.0" || got["helheim-go"].Dir != filepath.Join(store, "helheim-go") || len(got["midgard-react"].SHA256) != 64 || len(got) != 2 {
		t.Errorf("Realms = %+v", got)
	}
	if s, _ := LoadState(Dir(project, id)); s.Realm != "midgard-react" || s.RealmVersion != "0.1.0" || len(s.Realms) != 2 {
		t.Errorf("run.json realms = %+v", s.Realms)
	}
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() with the realms as they were = %v", err)
	}
	r.Close()
	prompt := filepath.Join(store, "helheim-go", "prompts")
	entries, _ := os.ReadDir(prompt)
	if err := os.WriteFile(filepath.Join(prompt, entries[0].Name()), []byte("changed under the run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, project, id, ""); err == nil || !strings.Contains(err.Error(), "the realm helheim-go changed since the run started") {
		t.Errorf("Open() after a prompt of a leaf's realm changed = %v", err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheFirstRunPinsTheLeadRealmItWasGivenByName(t *testing.T) {
	_, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0"})
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	r.Close()
	u, err := schema.LoadUmbrella(project)
	if err != nil || u.Realm != "midgard-react" || u.RealmVersion != "0.1.0" {
		t.Fatalf("umbrella.yaml = %+v, %v; want the lead realm pinned", u, err)
	}
	if msg := gitOut(t, project, "log", "-1", "--format=%s", "--", "umbrella.yaml"); msg != "Pin the lead realm midgard-react 0.1.0" {
		t.Errorf("the pin's commit says %q", msg)
	}
	if st := gitOut(t, project, "status", "--porcelain", "--", "umbrella.yaml"); st != "" {
		t.Errorf("umbrella.yaml is left uncommitted: %q", st)
	}
	ledger, _ := os.ReadFile(filepath.Join(Dir(project, r.State.ID), "ledger.jsonl"))
	first, _, _ := strings.Cut(string(ledger), "\n")
	if !strings.Contains(first, `"event":"run_started"`) || !strings.Contains(first, `"ygg.realm.name":"midgard-react"`) || !strings.Contains(first, `"ygg.realm.version":"0.1.0"`) ||
		!strings.Contains(first, "realms: midgard-react 0.1.0 sha256:") {
		t.Errorf("the first ledger line lacks the realms:\n%s", first)
	}
	// The next run finds the pin and agrees with it.
	r2, err := Start(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"})
	if err != nil {
		t.Fatalf("a second Start() = %v", err)
	}
	r2.Close()
	if n := gitOut(t, project, "rev-list", "--count", "HEAD", "--", "umbrella.yaml"); n != "2" {
		t.Errorf("umbrella.yaml has %s commits; the second run pinned nothing", n)
	}
}

func TestARealmGivenByPathIsNotPinned(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	before := gitOut(t, project, "rev-parse", "HEAD")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
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
	if _, err := Start(ctx, Options{Project: project, Config: cfg, Realm: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "umbrella.yaml") {
		t.Errorf("Start() = %v; the operator's own edit of umbrella.yaml is not Odin's to commit", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".factory", "runs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(project, ".factory", "runs"))
		if len(entries) != 0 {
			t.Errorf("a refused start left run directories: %v", entries)
		}
	}
}
