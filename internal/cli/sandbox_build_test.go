package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contextRealm writes a realm whose stack is built from devcontainer/Containerfile, with stages web and
// acceptance, and an acceptance stack on the second one.
func contextRealm(t *testing.T, store, name string) {
	t.Helper()
	dir := filepath.Join(store, name)
	files := map[string]string{
		"pack.yaml":                   "name: " + name + "\nversion: 0.1.0\nsandbox:\n  build: devcontainer\n  target: web\n  acceptance:\n    build: devcontainer\n    target: acceptance\n",
		"devcontainer/Containerfile":  "FROM " + goStack + " AS web\nCOPY checks/ /opt/ygg/checks/\nFROM web AS acceptance\nRUN true\n",
		"devcontainer/checks/unit.sh": "#!/bin/sh\ntrue\n",
	}
	for f, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSandboxBuildBuildsARealmsStackFromItsContextThenTheLayersOnIt(t *testing.T) {
	log := stubEngine(t)
	store, config := realmsStore(t)
	contextRealm(t, store, "midgard-react")
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", "midgard-react")
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	var stacks []string
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "podman build --pull=never -t localhost/ygg-stack-midgard-react") {
			stacks = append(stacks, line)
		}
	}
	if len(stacks) != 2 || !strings.Contains(stacks[0], "--target web") || !strings.Contains(stacks[1], "-acceptance:") || !strings.Contains(stacks[1], "--target acceptance") {
		t.Errorf("the realm's two stacks were built as:\n%s\nall calls:\n%s", strings.Join(stacks, "\n"), calls)
	}
	if got := builds(log); len(got) != 2 || !strings.Contains(strings.Join(got, "\n"), "STACK_IMAGE=localhost/ygg-stack-midgard-react:") {
		t.Errorf("the layers were built on:\n%s", strings.Join(got, "\n"))
	}
	if layers := builds(log); strings.Index(calls, "-t localhost/ygg-stack-") > strings.Index(calls, layers[0]) {
		t.Errorf("the factory layers were built before the stack:\n%s", calls)
	}
	if strings.Contains(calls, "podman pull -q --authfile") && !strings.Contains(calls, goStack) {
		t.Errorf("the Containerfile's pinned base was not pulled:\n%s", calls)
	}
	// A second build finds the stacks there and builds the layers' images again only.
	code, _, _ = ygg(t, "sandbox", "build", "--config", config, "--realm", "midgard-react")
	b, _ = os.ReadFile(log)
	if code != ExitDone || strings.Count(string(b), "podman build --pull=never -t localhost/ygg-stack-midgard-react") != 2 {
		t.Errorf("a stack that was built was built again (exit %d):\n%s", code, b)
	}
}
