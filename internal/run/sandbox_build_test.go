package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const goBase = "docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514"

// buildContext turns the realm name in store into one whose stack is built from its own devcontainer folder.
func buildContext(t *testing.T, store, name string) string {
	t.Helper()
	dir := filepath.Join(store, name)
	pack, err := os.ReadFile(filepath.Join(dir, "pack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(pack), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "  stack: ") {
			lines[i] = "  build: devcontainer"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "devcontainer", "checks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"Containerfile": "FROM " + goBase + "\nCOPY checks/ /opt/ygg/checks/\n", "checks/unit.sh": "#!/bin/sh\ntrue\n"} {
		if err := os.WriteFile(filepath.Join(dir, "devcontainer", f), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := schema.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := p.StackRef(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestARealmWhoseStackIsBuiltFromItsContextGivesItsLeavesTheTagOfTheBuild(t *testing.T) {
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", "")
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org", "")
	webRef := buildContext(t, store, "midgard-react")
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(s *SandboxSetup, _ string) (gungnir.Sandbox, error) {
		if want := sandbox.FactoryImages(); s.Config.Images != want {
			t.Errorf("the default images are %+v, want those of the factory layer %+v", s.Config.Images, want)
		}
		return fake, nil
	}}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: sandboxConfig(t, script, "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n"), Graph: "midgard-react", Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, repo := range []string{"repos/app", "repos/api"} {
		if _, err := r.Runner(ctx, repo); err != nil {
			t.Fatalf("Runner(%s) = %v", repo, err)
		}
	}
	stacks := map[string]string{}
	for _, s := range fake.Ups {
		stacks[s.Name] = s.Stack
		if err := s.Validate(); err != nil {
			t.Errorf("the spec for %s is one the driver would refuse: %v", s.Name, err)
		}
	}
	if stacks["repos/app"] != webRef || !strings.HasPrefix(webRef, "localhost/ygg-stack-midgard-react:") {
		t.Errorf("the web leaf starts on %q, want the local tag %q", stacks["repos/app"], webRef)
	}
	if stacks["repos/api"] != nodeStack {
		t.Errorf("the api leaf, whose realm names an image, starts on %q, want %q", stacks["repos/api"], nodeStack)
	}
}
