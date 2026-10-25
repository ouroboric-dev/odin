//go:build sandbox

package cli

// The escape-attempt tests of spec §13 that need a sandbox, through the whole stack: a scripted
// agent, inside a real sandbox on a real engine, tries to read the host's keys, open the network,
// plant a hook, and plant a devcontainer.json with initializeCommand in its leaf. Each test asserts
// that Odin blocks or ignores the attempt. They need what gleipnir's sandbox integration test needs:
//
//	YGG_SANDBOX_ENGINE, YGG_SANDBOX_TREE, YGG_SANDBOX_TREE_ENGINE (see gleipnir/sandbox/integration_test.go)
//
// and run with: go test -tags sandbox -run TestSandboxEscapes -v -timeout 40m ./internal/cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func TestSandboxEscapes(t *testing.T) {
	socket, tree, engineTree := os.Getenv("YGG_SANDBOX_ENGINE"), os.Getenv("YGG_SANDBOX_TREE"), os.Getenv("YGG_SANDBOX_TREE_ENGINE")
	if socket == "" || tree == "" || engineTree == "" {
		t.Skip("set YGG_SANDBOX_ENGINE, YGG_SANDBOX_TREE and YGG_SANDBOX_TREE_ENGINE to run the escape tests")
	}
	t.Setenv("TMPDIR", tree) // everything the test builds is a path the engine sees
	realm, _ := filepath.Abs("../../testdata/toy-sandbox")
	cfgDir := t.TempDir()
	marker := filepath.Join(cfgDir, "initializeCommand-ran-on-the-host")
	canaryDir := t.TempDir()
	canary := filepath.Join(canaryDir, "id_ed25519")
	os.WriteFile(canary, []byte("PRIVATE KEY\n"), 0o600)

	hostile := `steps:
  design:
    - actions:
        - run: { argv: [cat, ` + canary + `], allow_failure: true }
        - run: { argv: [sh, -c, "cat /home/*/.ssh/* 2>&1; ls -a ~/.ssh 2>&1; echo done"], allow_failure: true }
        - run: { argv: [bash, -c, "timeout 4 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo CONNECTED || echo BLOCKED"], allow_failure: true }
        - run: { argv: [sh, -c, "echo hook > ../../.git/modules/repos/app/hooks/pre-commit && echo WROTE || echo REFUSED"], allow_failure: true }
        - run: { argv: [sh, -c, "echo x > ../../features.json && echo WROTE || echo REFUSED"], allow_failure: true }
        - run: { argv: [sh, -c, "grep ^CapEff /proc/self/status"], allow_failure: true }
        - write: { path: .devcontainer/devcontainer.json, content: "{\"initializeCommand\": \"touch ` + marker + `\", \"image\": \"x\"}\n" }
      result: { status: ok, summary: tried every escape, output: '{"summary": "done"}' }
  build:
    - actions: [ { write: { path: build.txt, content: "done\n" } } ]
      result: { status: ok, summary: built, output: '{"summary": "done"}' }
    - actions: [ { write: { path: build.txt, content: "done\n" } } ]
      result: { status: ok, summary: built again, output: '{"summary": "done"}' }
`
	script := filepath.Join(cfgDir, "script.yaml")
	os.WriteFile(script, []byte(hostile), 0o644)
	config := filepath.Join(cfgDir, "factory.yaml")
	os.WriteFile(config, []byte("mode: unattended\ngates: { design: auto }\nroles:\n  planner: { agent: scripted, model: none, script: "+script+" }\n"+
		"  implementer: { agent: scripted, model: none, script: "+script+" }\n"+
		"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"+
		"sandbox:\n  engine: "+socket+"\n  mounts: [ { host: "+tree+", engine: "+engineTree+" } ]\n"), 0o644)

	if code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", realm); code != ExitDone {
		t.Fatalf("sandbox build: exit %d\n%s%s", code, out, errOut)
	}
	dir := treetest.Project(t)
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", realm)
	if code != ExitStopped && code != ExitDone {
		t.Fatalf("run: exit %d\n%s%s", code, out, errOut)
	}
	id := runID(t, dir)
	logs := func(n int) string {
		b, _ := os.ReadFile(filepath.Join(run.Dir(dir, id), "transcripts", "design-1-run-"+strconv.Itoa(n)+".log"))
		return string(b)
	}
	for i := 1; i <= 6; i++ {
		t.Logf("probe %d said: %s", i, strings.TrimSpace(logs(i)))
	}
	if got := logs(1); strings.Contains(got, "PRIVATE KEY") || !strings.Contains(got, "No such file") {
		t.Errorf("the agent read the host's key file: %q", got)
	}
	if got := logs(2); strings.Contains(got, "PRIVATE") || strings.Contains(got, "BEGIN") {
		t.Errorf("the agent found key material: %q", got)
	}
	if got := logs(3); !strings.Contains(got, "BLOCKED") || strings.Contains(got, "CONNECTED") {
		t.Errorf("the agent opened the network: %q", got)
	}
	if got := logs(4); !strings.Contains(got, "REFUSED") {
		t.Errorf("the agent planted a hook in the git dir: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "features.json")); err == nil {
		t.Error("the agent wrote the project's features.json")
	}
	if got := logs(6); !strings.Contains(got, "0000000000000000") {
		t.Errorf("the agent has capabilities: %q", got)
	}
	verifies(t, dir, id)

	// The agent's devcontainer.json, with initializeCommand, is now in the leaf. A second run starts a
	// new sandbox for the same leaf: it must use the factory's configuration, never the leaf's.
	if code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", realm); code != ExitStopped && code != ExitDone {
		t.Fatalf("the second run: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("initializeCommand ran on the host")
	}
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitDone || !strings.Contains(out, "0 container(s) of ygg runs") {
		t.Errorf("doctor after the runs: exit %d\n%s", code, out)
	}
}
