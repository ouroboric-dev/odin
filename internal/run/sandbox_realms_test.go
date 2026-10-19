package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const (
	nodeStack = "docker.io/library/node:24.1.0-bookworm@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	pgImage   = "docker.io/library/postgres:17.2@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s3Image   = "docker.io/chrislusf/seaweedfs:3.80@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// sandboxRealm copies the sandboxed toy realm into store as name, with the given lines after the sandbox
// stack, and a graph that also touches repos/api so both leaves are in the run.
func sandboxRealm(t *testing.T, store, name, version, stack, egress, services string) {
	t.Helper()
	dir := filepath.Join(store, name)
	if err := os.CopyFS(dir, os.DirFS(toySandbox(t))); err != nil {
		t.Fatal(err)
	}
	pack := "name: " + name + "\nversion: " + version + "\ngraph: graph.yaml\nchecks:\n  unit: [grep, -q, done, build.txt]\n" +
		"sandbox:\n  stack: " + stack + "\n  egress: [" + egress + "]\n" + services
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(filepath.Join(dir, "graph.yaml"))
	g = append(g, []byte("  - { id: api-touch, kind: exec, run: [echo, api], repo: repos/api }\n")...)
	os.WriteFile(filepath.Join(dir, "graph.yaml"), g, 0o644)
}

const apiServices = "services:\n  postgres:\n    image: " + pgImage + "\n    env: { POSTGRES_DB: app, POSTGRES_HOST_AUTH_METHOD: trust }\n    ports: [5432]\n" +
	"  seaweedfs:\n    image: " + s3Image + "\n    ports: [8333]\n"

// gallery starts a run of a web leaf and an api leaf, each in its own realm; the web leaf consumes the api.
func gallery(t *testing.T, webServices string) (*Run, *sandboxfake.Sandbox) {
	t.Helper()
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", "docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514",
		"registry.npmjs.org", webServices)
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org, sum.golang.org", apiServices)
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Consumes: []string{"repos/api"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "realms:\n  dir: "+store+"\n")
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: cfg, Realm: "midgard-react", Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, fake
}

func TestEachLeafsSandboxGetsItsRealmsStackAndEgress(t *testing.T) {
	r, fake := gallery(t, "")
	for _, repo := range []string{"repos/app", "repos/api"} {
		if _, err := r.Runner(ctx, repo); err != nil {
			t.Fatalf("Runner(%s) = %v", repo, err)
		}
	}
	specs := map[string]gungnir.SandboxSpec{}
	for _, s := range fake.Ups {
		specs[s.Name] = s
		if err := s.Validate(); err != nil {
			t.Errorf("the spec Odin built for %s is one the driver would refuse: %v", s.Name, err)
		}
	}
	web, api := specs["repos/app"], specs["repos/api"]
	if !strings.Contains(web.Stack, "golang") || !strings.Contains(api.Stack, "node:24.1.0") {
		t.Errorf("stacks: web %q, api %q; each leaf runs on the stack of its own realm", web.Stack, api.Stack)
	}
	if got := strings.Join(web.Egress, ","); got != "registry.npmjs.org" {
		t.Errorf("web egress = %s", got)
	}
	if got := strings.Join(api.Egress, ","); got != "proxy.golang.org,sum.golang.org" {
		t.Errorf("api egress = %s", got)
	}
}

func TestASandboxGetsTheServicesOfEachLeafItConsumes(t *testing.T) {
	r, fake := gallery(t, "")
	for _, repo := range []string{"repos/app", "repos/api"} {
		if _, err := r.Runner(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	names := func(s gungnir.SandboxSpec) string {
		var out []string
		for _, v := range s.Services {
			out = append(out, v.Name)
		}
		return strings.Join(out, ",")
	}
	var web, api gungnir.SandboxSpec
	for _, s := range fake.Ups {
		if s.Name == "repos/app" {
			web = s
		} else {
			api = s
		}
	}
	if names(api) != "postgres,seaweedfs" {
		t.Errorf("api services = %s; its own realm's", names(api))
	}
	if names(web) != "postgres,seaweedfs" {
		t.Errorf("web services = %s; it consumes the api, so it gets the services of the api's realm (spec §9.2)", names(web))
	}
	if web.Services[0].Image != pgImage || web.Services[0].Env["POSTGRES_DB"] != "app" || web.Services[0].Ports[0] != 5432 {
		t.Errorf("postgres = %+v", web.Services[0])
	}
}

func TestTwoRealmsOfferingDifferentServicesOfOneNameStopTheRunBeforeASandboxStarts(t *testing.T) {
	store := t.TempDir()
	clash := "services:\n  postgres:\n    image: " + pgImage + "\n    env: { POSTGRES_DB: other }\n    ports: [5432]\n"
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", clash)
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org", apiServices)
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Consumes: []string{"repos/api"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "realms:\n  dir: "+store+"\n")
	_, err := Prepare(ctx, Options{Project: treetest.Build(t, root), Config: cfg, Realm: "midgard-react"})
	if err == nil || !strings.Contains(err.Error(), "two different services named postgres") {
		t.Errorf("Prepare() = %v", err)
	}
}

func TestARealmThatNamesNoStackCannotProvideALeafWhenASandboxIsOn(t *testing.T) {
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", "")
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org", "")
	pack := filepath.Join(store, "helheim-go", "pack.yaml")
	b, _ := os.ReadFile(pack)
	os.WriteFile(pack, []byte(strings.SplitN(string(b), "sandbox:", 2)[0]), 0o644)
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	_, err := Prepare(ctx, Options{Project: treetest.Build(t, root), Config: sandboxConfig(t, script, "realms:\n  dir: "+store+"\n"), Realm: "midgard-react"})
	if err == nil || !strings.Contains(err.Error(), "helheim-go") || !strings.Contains(err.Error(), "repos/api") || !strings.Contains(err.Error(), "no sandbox stack") {
		t.Errorf("Prepare() = %v", err)
	}
}
