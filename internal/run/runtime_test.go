package run

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// runtimeRealm writes a runtime realm into the store: a service for each need it provides, and provides.
func runtimeRealm(t *testing.T, store, name, version string, provides ...string) {
	t.Helper()
	images := map[string]string{"postgres": pgImage, "s3": s3Image}
	ports := map[string]int{"postgres": 5432, "s3": 8333}
	pack := "name: " + name + "\nversion: " + version + "\n"
	if len(provides) > 0 {
		pack += "services:\n"
		for _, n := range provides {
			pack += fmt.Sprintf("  %s:\n    image: %s\n    ports: [%d]\n", n, images[n], ports[n])
		}
		pack += "provides:\n"
		for _, n := range provides {
			pack += "  " + n + ": " + n + "\n"
		}
	}
	dir := filepath.Join(store, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
}

// needing makes the realm called name in store need needs.
func needing(t *testing.T, store, name string, needs ...string) {
	t.Helper()
	pack := filepath.Join(store, name, "pack.yaml")
	b, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, append(b, "needs: ["+strings.Join(needs, ", ")+"]\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withRuntime is project node n pinning the runtime realm name at version.
func withRuntime(n treetest.Node, name, version string) treetest.Node {
	n.RuntimePin = treetest.Pin{Realm: name, Version: version}
	return n
}

// apiProject is a project of the toy graph's repos/app and an api leaf of helheim-go.
func apiProject() treetest.Node {
	return pinned("midgard-react", "0.1.0", map[string]treetest.Pin{
		"repos/app": {Realm: "midgard-react", Version: "0.1.0"}, "repos/api": {Realm: "helheim-go", Version: "0.2.0"}})
}

func TestRuntimeRealmIsLoaded(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	runtimeRealm(t, store, "niflheim-local", "0.1.0", "postgres", "s3")

	// A project whose umbrella pins a runtime realm gets it, and run.json and the first ledger line record it.
	project := treetest.Build(t, withRuntime(apiProject(), "niflheim-local", "0.1.0"))
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if r.Setup.Runtime == nil || r.Setup.Runtime.Name != "niflheim-local" || r.Setup.Runtime.Version != "0.1.0" {
		t.Errorf("Setup.Runtime = %+v, want niflheim-local 0.1.0", r.Setup.Runtime)
	}
	id := r.State.ID
	r.Close()
	if s, _ := LoadState(Dir(project, id)); s.Realms["niflheim-local"].Version != "0.1.0" || s.Realms["niflheim-local"].Dir != filepath.Join(store, "niflheim-local") || len(s.Realms["niflheim-local"].SHA256) != 64 {
		t.Errorf("run.json realms = %+v", s.Realms)
	}
	ledger, _ := os.ReadFile(filepath.Join(Dir(project, id), "ledger.jsonl"))
	if first, _, _ := strings.Cut(string(ledger), "\n"); !strings.Contains(first, "niflheim-local 0.1.0 sha256:") {
		t.Errorf("the first ledger line lacks the runtime realm:\n%s", first)
	}
	again, err := Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if again.Setup.Runtime == nil || again.Setup.Runtime.Name != "niflheim-local" {
		t.Errorf("a resumed run's runtime = %+v", again.Setup.Runtime)
	}
	again.Close()

	// A project that pins none has none, and takes the one a leaves file names when init adds it.
	plain := treetest.Build(t, apiProject())
	r, err = Start(ctx, Options{Project: plain, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() with no runtime = %v", err)
	}
	defer r.Close()
	if r.Setup.Runtime != nil {
		t.Errorf("Setup.Runtime = %+v for a project that pins none", r.Setup.Runtime)
	}
	if err := r.AddRealms([]schema.Pin{{Realm: "niflheim-local", Version: "0.1.0", Where: schema.RuntimeWhere}}); err != nil {
		t.Fatalf("AddRealms(runtime) = %v", err)
	}
	if r.Setup.Runtime == nil || r.Setup.Runtime.Name != "niflheim-local" || r.State.Realms["niflheim-local"].Version != "0.1.0" {
		t.Errorf("after AddRealms: Setup.Runtime = %+v, run.json realms %+v", r.Setup.Runtime, r.State.Realms)
	}
	ledger, _ = os.ReadFile(filepath.Join(r.Dir, "ledger.jsonl"))
	if strings.Count(string(ledger), `"event":"realm_loaded"`) != 1 || !strings.Contains(string(ledger), `"ygg.realm.name":"niflheim-local"`) {
		t.Errorf("the ledger should hold one realm_loaded, for the runtime realm:\n%s", ledger)
	}

	// A pin the realm store does not hold fails before anything is created, as a leaf's pin does.
	for pin, want := range map[treetest.Pin]string{
		{Realm: "niflheim-local", Version: "0.9.0"}: "the realm store holds niflheim-local 0.1.0, but runtime pins 0.9.0",
		{Realm: "asgard-cloud", Version: "0.1.0"}:   "runtime pins realm asgard-cloud, which can't be loaded",
	} {
		bad := treetest.Build(t, withRuntime(apiProject(), pin.Realm, pin.Version))
		if _, err := Start(ctx, Options{Project: bad, Config: cfg, Graph: "midgard-react"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s %s: Start() = %v, want %q", pin.Realm, pin.Version, err, want)
		}
		if _, err := os.Stat(filepath.Join(bad, ".factory")); err == nil {
			t.Errorf("%s %s: a refused start created .factory/", pin.Realm, pin.Version)
		}
	}
}

// unmet checks that err refuses for the need of the realm, in the words the operator acts on.
func unmet(t *testing.T, what string, err error, need, realmName string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want a refusal for the need %s of %s", what, need, realmName)
	}
	for _, want := range []string{"the realm " + realmName + " needs " + need, "runtime in docs/leaves.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %q does not say %q", what, err, want)
		}
	}
}

func TestNeedsWithoutAProviderStop(t *testing.T) {
	store, cfg := realmStore(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.2.0"})
	needing(t, store, "helheim-go", "postgres", "s3")
	runtimeRealm(t, store, "niflheim-local", "0.1.0", "postgres")
	if err := os.CopyFS(filepath.Join(store, "helheim-toy"), os.DirFS("../../testdata/realms/helheim-toy")); err != nil {
		t.Fatal(err)
	}

	// A leaf realm that needs postgres and s3, in a project with no runtime realm, stops the run before
	// anything is created.
	project := treetest.Build(t, apiProject())
	_, err := Start(ctx, Options{Project: project, Config: cfg, Graph: "midgard-react"})
	unmet(t, "no runtime realm", err, "postgres", "helheim-go")
	unmet(t, "no runtime realm", err, "s3", "helheim-go")
	if _, serr := os.Stat(filepath.Join(project, ".factory")); serr == nil {
		t.Error("a refused start created .factory/")
	}

	// What init calls, for the realms of the leaves it is about to create: CheckNeeds with the project's runtime
	// realm, CheckNeedsWith with the one the leaves file pins, and PlanRealms for the pin itself.
	plain := treetest.Build(t, pinned("midgard-react", "0.1.0", map[string]treetest.Pin{"repos/app": {Realm: "midgard-react", Version: "0.1.0"}}))
	runtimeRealm(t, store, "niflheim-full", "0.1.0", "postgres", "s3")
	r, err := Start(ctx, Options{Project: plain, Config: cfg, Graph: "midgard-react"})
	if err != nil {
		t.Fatalf("Start() of a project whose realms need nothing = %v", err)
	}
	r.Close() // the run's setup is all this needs
	api, err := realm.Load(filepath.Join(store, "helheim-go"))
	if err != nil {
		t.Fatal(err)
	}
	unmet(t, "init's realms, no runtime", r.Setup.CheckNeeds([]*realm.Realm{api}), "s3", "helheim-go")
	full, err := realm.Load(filepath.Join(store, "niflheim-full"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Setup.CheckNeedsWith(full, []*realm.Realm{api}); err != nil {
		t.Errorf("CheckNeedsWith(a runtime realm that provides both) = %v", err)
	}
	if _, _, err := r.PlanRealms([]schema.Pin{{Realm: "helheim-toy", Version: "0.1.0", Where: schema.RuntimeWhere}}); err == nil || !strings.Contains(err.Error(), "build realm") {
		t.Errorf("PlanRealms(a build realm as the runtime) = %v", err)
	}

	// The realm of a leaf Start would create counts too: nothing is created.
	needing(t, store, "midgard-react", "postgres")
	empty := treetest.Build(t, pinned("midgard-react", "0.1.0", nil))
	_, err = Start(ctx, Options{Project: empty, Config: cfg, Graph: "midgard-react"})
	unmet(t, "a leaf Start creates", err, "postgres", "midgard-react")
	if u, _ := schema.LoadUmbrella(empty); len(u.Children) != 0 {
		t.Errorf("the project lists %v after a refused start", u.Paths())
	}

	// A runtime realm that provides only postgres stops it, naming s3 alone.
	partial := treetest.Build(t, withRuntime(apiProject(), "niflheim-local", "0.1.0"))
	_, err = Start(ctx, Options{Project: partial, Config: cfg, Graph: "midgard-react"})
	unmet(t, "a runtime realm without s3", err, "s3", "helheim-go")
	if err != nil && (strings.Contains(err.Error(), "needs postgres") || !strings.Contains(err.Error(), "niflheim-local")) {
		t.Errorf("the refusal %q names a need the runtime realm provides, or not the runtime realm", err)
	}

	// A runtime pin in the umbrella that names a build realm (one with a leaf recipe) is refused.
	build := treetest.Build(t, withRuntime(apiProject(), "helheim-toy", "0.1.0"))
	if _, err := Start(ctx, Options{Project: build, Config: cfg, Graph: "midgard-react"}); err == nil || !strings.Contains(err.Error(), "helheim-toy") || !strings.Contains(err.Error(), "build realm") {
		t.Errorf("a build realm as the runtime: Start() = %v", err)
	}
}
