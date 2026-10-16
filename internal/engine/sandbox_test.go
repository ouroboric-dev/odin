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
	"github.com/ouroboric-dev/odin/internal/skillfake"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const sandboxPack = "checks:\n  unit: [grep, -q, done, build.txt]\n  junk: [sh, -c, \"echo scratch > junk.txt; grep -q done build.txt\"]\n  plant: [sh, -c, \"mkdir -p vendor/x/.git && grep -q done build.txt\"]\n" +
	"leaves:\n  repos/app: { description: the app, template: t, first_commit: [LICENSE] }\n" +
	"sandbox:\n  stack: docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514\n  egress: [proxy.golang.org]\n"

// startSandboxed starts a run whose leaf commands go through a fake sandbox.
func startSandboxed(t *testing.T, nodes, script string) (*run.Run, *sandboxfake.Sandbox) {
	t.Helper()
	return startSandboxedWith(t, nodes, script, run.Hooks{})
}

// startSandboxedWith is startSandboxed with other hooks, such as a skill library.
func startSandboxedWith(t *testing.T, nodes, script string, h run.Hooks) (*run.Run, *sandboxfake.Sandbox) {
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
	h.NewSandbox = func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }
	r, err := run.Start(context.Background(), run.Options{Project: project, Config: filepath.Join(cfg, "factory.yaml"), Realm: realm, Hooks: h})
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
	if fake.Scans != 2 {
		t.Errorf("%d secret scans, want one before each of the two checkpoints (the agent's and the exec's)", fake.Scans)
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
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the sandbox didn't start") || r2.State.Attempts["a"] != 1 {
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

func TestAnAgentSessionGetsTheSandboxItsSkillsAndReportsItsProvider(t *testing.T) {
	script := "steps:\n  a:\n    - actions:\n        - run: { argv: [touch, from-the-agent.txt] }\n        - write: { path: build.txt, content: \"done\\n\" }\n" +
		"      result: { status: ok, summary: built, provider: anthropic, cli_version: 2.1.284, usage: { in: 10, out: 5, turns: 1, cost_usd: 0.01 } }\n"
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, skills: [plain-language] }\n"
	r, fake := startSandboxedWith(t, nodes, script, run.Hooks{Libs: skillfake.Libraries(t, "plain-language", "Use short words.")})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(fake.Execs) != 1 || fake.Execs[0].Argv[0] != "touch" {
		t.Errorf("the agent's command went %+v; want one, through the sandbox", fake.Execs)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "from-the-agent.txt")); err != nil {
		t.Errorf("the sandbox didn't act on the leaf: %v", err)
	}
	tr, _ := os.ReadFile(filepath.Join(r.Dir, "transcripts", "a-1.json"))
	if !strings.Contains(string(tr), "Do the work.") || !strings.Contains(string(tr), "### plain-language") || !strings.Contains(string(tr), "Use short words.") {
		t.Errorf("the prompt = %s; want the node's prompt and its skill", tr)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"gen_ai.provider.name":"anthropic"`, `"ygg.agent.cli_version":"2.1.284"`, `"event":"sandbox_up"`, `"event":"sandbox_down"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s", want)
		}
	}
}

func TestAnAgentThatNamesNoSkillsLoadsNone(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"
	r, _ := startSandboxedWith(t, nodes, buildScript, run.Hooks{Libs: skillfake.Libraries(t, "plain-language", "Use short words.")})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	tr, _ := os.ReadFile(filepath.Join(r.Dir, "transcripts", "a-1.json"))
	if strings.Contains(string(tr), "plain-language") {
		t.Errorf("a skill the node didn't name reached the prompt: %s", tr)
	}
}

const secretReport = `[{"RuleID":"aws-access-token","File":"/config/prod.env","StartLine":3,"Secret":"REDACTED","Match":"REDACTED"}]`

func TestASecretInAnAgentsWorkStopsTheRunBeforeAnythingIsCommitted(t *testing.T) {
	script := buildScript + "    - actions: [ { write: { path: build.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: built again }\n"
	r, fake := startSandboxed(t, buildNodes, script)
	fake.Scan.Exit, fake.Scan.Report = 7, secretReport
	before := r.State.Leaves["repos/app"].Checkpoint
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateRealSecrets {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at real-secrets-or-user-data", out, err, r.State.PendingGate)
	}
	if got := r.State.Leaves["repos/app"].Checkpoint; got != before {
		t.Errorf("the checkpoint moved to %s: a secret was committed", got)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "a-1.md"))
	if !strings.Contains(string(gate), "config/prod.env:3 (aws-access-token)") || strings.Contains(string(gate), "REDACTED") {
		t.Errorf("the gate file = %s; want the rule, file and line, and no secret", gate)
	}
	// The leaf stays as the node left it, for the person to look at.
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "build.txt")); err != nil {
		t.Error("the quarantined leaf lost the agent's file")
	}
	// Opening the run again, as a resume or an approve does, leaves the quarantined leaf as it is.
	id := r.State.ID
	r.Close()
	again, err := run.OpenWith(context.Background(), r.Project, id, "", run.Hooks{NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	if _, err := os.Stat(filepath.Join(again.RepoDir("repos/app"), "build.txt")); err != nil {
		t.Error("opening the run reset a leaf that was kept for the person to look at")
	}
	r = again
	// Approving puts the leaf back, without saving the secret anywhere, and runs the node again.
	fake.Scan.Exit, fake.Scan.Report = 0, ""
	if err := Approve(context.Background(), r, "a", "false positive: a test key"); err != nil {
		t.Fatal(err)
	}
	if refs, _ := git.Run(context.Background(), r.RepoDir("repos/app"), "-c", "safe.directory=*", "--git-dir="+r.Setup.Repos["repos/app"].GitDir, "for-each-ref", "refs/ygg/"); refs != "" {
		t.Errorf("approving saved the discarded work in %q: a secret would be in a ref", refs)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Errorf("the second Advance() = %+v, %v", out, err)
	}
}

func TestAScanThatCannotRunIsNeverACleanTree(t *testing.T) {
	r, fake := startSandboxed(t, buildNodes, buildScript)
	fake.Scan.Exit = 3
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the secret scan failed") {
		t.Fatalf("Advance() = %+v, %v; want a failed node", out, err)
	}
	if r.State.Leaves["repos/app"].Checkpoint != r.State.Leaves["repos/app"].Base {
		t.Error("a checkpoint was committed without a scan")
	}
}

func TestAReadOnlyRoleThatChangesTheLeafIsAHardStop(t *testing.T) {
	r, _ := startSandboxed(t, "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, read_only: true }\n", buildScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if got := r.State.Leaves["repos/app"].Checkpoint; got != r.State.Leaves["repos/app"].Base {
		t.Error("a read-only role's change was committed")
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "a-1.md"))
	if !strings.Contains(string(gate), "read-only role planner changed the leaf") {
		t.Errorf("the gate file = %s", gate)
	}
}
