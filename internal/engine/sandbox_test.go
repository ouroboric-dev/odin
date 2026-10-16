package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const sandboxPack = "checks:\n  unit: [grep, -q, done, build.txt]\n  junk: [sh, -c, \"echo scratch > junk.txt; grep -q done build.txt\"]\n  plant: [sh, -c, \"mkdir -p vendor/x/.git && grep -q done build.txt\"]\n" +
	"leaves:\n  repos/app: { description: the app, template: t, first_commit: [LICENSE] }\n" +
	"sandbox:\n  stack: docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514\n  egress: [proxy.golang.org]\n"

// startSandboxed starts a run whose leaf commands go through a fake sandbox.
func startSandboxed(t *testing.T, nodes, script string) (*run.Run, *sandboxfake.Sandbox) {
	t.Helper()
	realm, cfg := t.TempDir(), t.TempDir()
	project := treetest.Project(t, "repos/app")
	tmpl := filepath.Join(realm, "t")
	os.MkdirAll(tmpl, 0o755)
	files := map[string]string{
		filepath.Join(tmpl, "LICENSE"):     "MIT\n",
		filepath.Join(realm, "pack.yaml"):  "name: test\nversion: 0.1.0\ngraph: graph.yaml\n" + sandboxPack,
		filepath.Join(realm, "graph.yaml"): "name: test\nversion: 1.0.0\nnodes:\n" + nodes,
		filepath.Join(realm, "p.md"):       "Do the work.\n",
		filepath.Join(cfg, "script.yaml"):  script,
		filepath.Join(cfg, "factory.yaml"): "mode: unattended\ngates: { design: auto }\nroles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n" +
			"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n" +
			"sandbox:\n  engine: unix:///run/ygg-test.sock\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := &sandboxfake.Sandbox{}
	r, err := run.Start(context.Background(), run.Options{Project: project, Config: filepath.Join(cfg, "factory.yaml"), Realm: realm,
		Hooks: run.Hooks{NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, fake
}

const buildScript = "steps:\n  a:\n    - actions: [ { write: { path: build.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: built }\n"
const buildNodes = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"

func leafStatus(t *testing.T, r *run.Run) string {
	t.Helper()
	nodes, err := tree.Resolve(context.Background(), r.Setup.Root, filepath.Join(r.Setup.ProjectPath, "repos/app"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := git.Repo{GitDir: nodes[1].GitDir, WorkTree: nodes[1].Dir}.Run(context.Background(), "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLeafCommandsRunInTheSandboxAndUmbrellaCommandsOnTheHost(t *testing.T) {
	r, fake := startSandboxed(t, buildNodes+
		"  - { id: check, kind: exec, run: [grep, -q, done, build.txt], repo: repos/app }\n"+
		"  - { id: gen, kind: exec, run: [\"true\"], repo: . }\n", buildScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(fake.Execs) != 1 || fake.Execs[0].Argv[0] != "grep" || len(fake.Execs[0].Env) != 0 {
		t.Errorf("sandbox commands = %+v; want only the leaf's check, with no environment of its own", fake.Execs)
	}
	if len(fake.Ups) != 1 || len(fake.Downs) != 1 {
		t.Errorf("ups %d, downs %d; want one sandbox, stopped when Advance returned", len(fake.Ups), len(fake.Downs))
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "logs", "check-1.log")); len(b) != 0 {
		t.Errorf("the log of a quiet check = %q", b)
	}
}

func TestChecksRunInTheSandboxAndTheLeafIsTidiedAfterThem(t *testing.T) {
	r, fake := startSandboxed(t, buildNodes+
		"  - { id: merge, kind: merge, repo: repos/app, checks: [junk] }\n"+
		"  - { id: pointer, kind: pointer, repo: repos/app, checks: [junk] }\n", buildScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := fake.ExecCount(); got != 2 {
		t.Errorf("%d commands went through the sandbox, want the merge's and the pointer's checks", got)
	}
	// B-004: what the checks wrote doesn't stay in the leaf, so the next run can start its branch.
	if st := leafStatus(t, r); st != "" {
		t.Errorf("the leaf has %q left by the checks", st)
	}
	r.Close()
	again, err := run.Start(context.Background(), run.Options{Project: r.Project, Config: r.Setup.ConfigPath, Realm: r.Setup.RealmDir,
		Hooks: run.Hooks{NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}})
	if err != nil {
		t.Errorf("the next run couldn't start: %v", err)
	} else {
		again.Close()
	}
}

// The merge node's own checks are tidied, whatever the pointer node does after them.
func TestAMergesChecksTidyTheLeaf(t *testing.T) {
	r, _ := startSandboxed(t, buildNodes+"  - { id: merge, kind: merge, repo: repos/app, checks: [junk] }\n", buildScript)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if st := leafStatus(t, r); st != "" {
		t.Errorf("the merge node's checks left %q in the leaf", st)
	}
}

func TestAPlantedRepositoryFromAChecksStopsTheRunAndIsKept(t *testing.T) {
	r, _ := startSandboxed(t, buildNodes+"  - { id: merge, kind: merge, repo: repos/app, checks: [plant] }\n", buildScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "vendor/x/.git")); err != nil {
		t.Error("tidy erased the planted repository, which is the evidence")
	}
}

func TestASandboxThatBreaksTheRulesIsAHardStopAndOneThatWontStartAFailure(t *testing.T) {
	r, fake := startSandboxed(t, buildNodes+"  - { id: check, kind: exec, run: [\"true\"], repo: repos/app }\n", buildScript)
	fake.UpErr = &sandbox.PolicyError{Problems: []string{"devcontainer.json has initializeCommand"}}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("a hostile config: %+v, %v, gate %s", out, err, r.State.PendingGate)
	}
	r2, fake2 := startSandboxed(t, buildNodes+"  - { id: check, kind: exec, run: [\"true\"], repo: repos/app }\n", buildScript)
	fake2.UpErr = errors.New("podman: connection refused")
	out, err = Advance(context.Background(), r2)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the sandbox didn't start") || r2.State.Attempts["check"] != 1 {
		t.Errorf("an engine that is down: %+v, %v; want a failed run, one attempt, no retry loop", out, err)
	}
}

func TestACommandThatRunsTooLongIsStoppedInTheSandbox(t *testing.T) {
	r, fake := startSandboxed(t, buildNodes+"  - { id: slow, kind: exec, run: [sleep, \"30\"], repo: repos/app, timeout: 1s }\n", buildScript)
	began := time.Now()
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "timed out after 1s") || time.Since(began) > 20*time.Second {
		t.Errorf("Advance() = %+v, %v after %v", out, err, time.Since(began))
	}
	if len(fake.Downs) != 1 {
		t.Error("the sandbox of a failed run was left up")
	}
}
