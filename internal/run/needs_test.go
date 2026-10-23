package run

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/services"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// needsProject builds the project and the config of needsRun, and pins the runtime realm when pin is set. init
// adds an init node to the graph, ahead of every node of the run.
func needsProject(t *testing.T, consumes, pin, init bool) (Options, *sandboxfake.Sandbox) {
	t.Helper()
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", "")
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org", "")
	needing(t, store, "midgard-react", "s3")
	needing(t, store, "helheim-go", "postgres", "s3")
	withAcceptance(t, store, "midgard-react")
	runtimeRealm(t, store, "niflheim-local", "0.1.0", "postgres", "s3")
	if init {
		graph := filepath.Join(store, "midgard-react", "graph.yaml")
		g, _ := os.ReadFile(graph)
		g = []byte(strings.Replace(string(g), "nodes:\n", "nodes:\n  - { id: leaves, kind: init, from: docs/leaves.yaml }\n", 1))
		if err := os.WriteFile(graph, g, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}}
	if consumes {
		app.Consumes = []string{"repos/api"}
	}
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": app,
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	if pin {
		root = withRuntime(root, "niflheim-local", "0.1.0")
	}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n")
	return Options{Project: treetest.Build(t, root), Config: cfg, Graph: "midgard-react", Hooks: h}, fake
}

// needsRun starts a run of a web leaf of midgard-react, which needs s3, and an api leaf of helheim-go, which
// needs postgres and s3, in a project that pins niflheim-local to provide both. Neither realm names a service
// of its own. consumes makes the web leaf consume the api.
func needsRun(t *testing.T, consumes bool) (*Run, *sandboxfake.Sandbox) {
	t.Helper()
	o, fake := needsProject(t, consumes, true, false)
	r, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, fake
}

func serviceNames(ss []gungnir.Service) string {
	var names []string
	for _, s := range ss {
		names = append(names, s.Name+"="+s.Image)
	}
	return strings.Join(names, ",")
}

func TestLeafSandboxGetsTheProvidersOfItsNeeds(t *testing.T) {
	r, fake := needsRun(t, true)
	both := "postgres=" + pgImage + ",s3=" + s3Image
	for _, repo := range []string{"repos/api", "repos/app"} {
		_, _, got, err := r.Setup.SandboxFor(repo)
		if err != nil {
			t.Fatalf("SandboxFor(%s) = %v", repo, err)
		}
		want, err := services.For(r.Setup.Project.Umbrella, []string{repo}, setupLoader{r.Setup})
		if err != nil || !reflect.DeepEqual(got, want) || serviceNames(got) != both {
			t.Errorf("SandboxFor(%s) gives %s; services.For gives %s (%v); want %s from niflheim-local", repo, serviceNames(got), serviceNames(want), err, both)
		}
		if _, err := r.Runner(ctx, repo); err != nil {
			t.Fatalf("Runner(%s) = %v", repo, err)
		}
	}
	for _, up := range fake.Ups {
		if serviceNames(up.Services) != both {
			t.Errorf("the sandbox of %s started %s, want %s", up.Name, serviceNames(up.Services), both)
		}
		if err := up.Validate(); err != nil {
			t.Errorf("the spec of %s is one the driver would refuse: %v", up.Name, err)
		}
	}
	// A web leaf that does not consume the api gets the provider of its own realm's need alone.
	r, _ = needsRun(t, false)
	if _, _, got, err := r.Setup.SandboxFor("repos/app"); err != nil || serviceNames(got) != "s3="+s3Image {
		t.Errorf("SandboxFor(repos/app) = %s, %v; want the s3 alone", serviceNames(got), err)
	}
}

func TestSnapshotSandboxGetsTheProvidersOfEveryLeafsNeeds(t *testing.T) {
	r, fake := needsRun(t, false)
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err != nil {
		t.Fatalf("SnapshotRunner() = %v", err)
	}
	if len(fake.Ups) != 1 {
		t.Fatalf("%d sandboxes came up", len(fake.Ups))
	}
	up := fake.Ups[0]
	if got, want := serviceNames(up.Services), "postgres="+pgImage+",s3="+s3Image; got != want {
		t.Errorf("the snapshot's sandbox started %s, want %s: the providers of every need of every leaf's realm, once each", got, want)
	}
	if err := up.Validate(); err != nil {
		t.Errorf("the spec is one the driver would refuse: %v", err)
	}
}

// With no runtime realm, a leaf's sandbox has no provider for its realm's needs: the run stops before any
// sandbox starts, unless an init node is ahead, which pins the runtime realm the leaves file names and refuses
// an unmet need before any leaf sandbox starts.
func TestALeafSandboxWithAnUnmetNeedNeverStarts(t *testing.T) {
	o, fake := needsProject(t, true, false, false)
	if _, err := Start(ctx, o); err == nil || !strings.Contains(err.Error(), "the realm helheim-go needs postgres") || len(fake.Ups) != 0 {
		t.Errorf("no runtime realm: Start() = %v, %d sandboxes up; want the unmet need and none", err, len(fake.Ups))
	}
	o, fake = needsProject(t, true, false, true)
	r, err := Start(ctx, o)
	if err != nil {
		t.Fatalf("an init node ahead: Start() = %v; init checks the needs, with the runtime realm it pins", err)
	}
	defer r.Close()
	if _, err := r.Runner(ctx, "repos/app"); err == nil || !strings.Contains(err.Error(), "the realm midgard-react needs s3 (an S3 API), and the project pins no runtime realm") || len(fake.Ups) != 0 {
		t.Errorf("a leaf sandbox before init pins the runtime realm: Runner() = %v, %d sandboxes up; want the unmet need and none", err, len(fake.Ups))
	}
}
