package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	goStack   = "docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514"
	nodeStack = "docker.io/library/node:24.1.0-bookworm@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	pgImage   = "docker.io/library/postgres:17.2@sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// realmsStore writes a realm store: midgard-react and helheim-go on stacks of their own, a third realm on
// the first one's stack, and a documents realm with no stack. It returns the store and a config for it.
func realmsStore(t *testing.T) (store, config string) {
	t.Helper()
	store = t.TempDir()
	for name, stack := range map[string]string{"midgard-react": nodeStack, "helheim-go": goStack, "midgard-vue": nodeStack, "asgard": ""} {
		body := "name: " + name + "\nversion: 0.1.0\n"
		if stack != "" {
			body += "sandbox:\n  stack: " + stack + "\n"
		}
		if name != "midgard-react" { // two realms share one service image, a third has none
			body += "services:\n  postgres:\n    image: " + pgImage + "\n    ports: [5432]\n"
		}
		if err := os.MkdirAll(filepath.Join(store, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, name, "pack.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, plain := project(t, "script.yaml", "")
	b, _ := os.ReadFile(plain)
	config = filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(config, append(b, []byte(sandboxExtra+"realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return store, config
}

func builds(log string) []string {
	b, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "--target work") {
			out = append(out, line)
		}
	}
	return out
}

func pulls(log string) []string {
	b, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "podman pull ") && strings.HasSuffix(line, pgImage) {
			out = append(out, line)
		}
	}
	return out
}

func TestSandboxBuildBuildsTheStackOfEveryRealmNamed(t *testing.T) {
	log := stubEngine(t)
	_, config := realmsStore(t)
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", "midgard-react", "--realm", "helheim-go")
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	got := builds(log)
	if len(got) != 2 || !strings.Contains(strings.Join(got, "\n"), "STACK_IMAGE="+nodeStack) || !strings.Contains(strings.Join(got, "\n"), "STACK_IMAGE="+goStack) {
		t.Errorf("work images built:\n%s", strings.Join(got, "\n"))
	}
	if strings.Count(out, "Built:") != 2 {
		t.Errorf("output:\n%s", out)
	}
	if got := pulls(log); len(got) != 1 || !strings.Contains(out, "Pulling the service image "+pgImage) {
		t.Errorf("the service image was pulled %d times; helheim-go has one, midgard-react none:\n%s", len(got), out)
	}
}

func TestSandboxBuildAllBuildsEachStackOnceAndSkipsRealmsWithNone(t *testing.T) {
	log := stubEngine(t)
	_, config := realmsStore(t)
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--all")
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if got := builds(log); len(got) != 2 {
		t.Errorf("%d work images built, want one for each of the two stacks (midgard-vue shares midgard-react's):\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(out, "asgard names no sandbox stack; skipped") {
		t.Errorf("output lacks the skipped realm:\n%s", out)
	}
	if got := pulls(log); len(got) != 1 {
		t.Errorf("the service image shared by two realms was pulled %d times, want once:\n%s", len(got), strings.Join(got, "\n"))
	}
}

func TestSandboxBuildNeedsARealmOrAllAndAStoreForNames(t *testing.T) {
	stubEngine(t)
	store, config := realmsStore(t)
	_ = store
	_, noStore := project(t, "script.yaml", "")
	noStore = withSandbox(t, noStore)
	for name, args := range map[string][]string{
		"neither":                     {"sandbox", "build", "--config", config},
		"all with a realm":            {"sandbox", "build", "--config", config, "--all", "--realm", "midgard-react"},
		"a name the store lacks":      {"sandbox", "build", "--config", config, "--realm", "nowhere"},
		"a name with no store":        {"sandbox", "build", "--config", noStore, "--realm", "midgard-react"},
		"all with no store":           {"sandbox", "build", "--config", noStore, "--all"},
		"a named realm with no stack": {"sandbox", "build", "--config", config, "--realm", "asgard"},
	} {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, code, ExitRefused)
		}
	}
}
