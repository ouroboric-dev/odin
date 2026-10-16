package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// start makes a realm with the given nodes and a prompt file, a script and a factory.yaml, and
// starts a run on them in a fresh project, whose nodes work in the leaf repos/app.
func start(t *testing.T, mode, gates, nodes, script string) *run.Run {
	t.Helper()
	return startPack(t, mode, gates, "", nodes, script)
}

// startPack is start with extra lines for the realm's pack.yaml, such as its checks.
func startPack(t *testing.T, mode, gates, pack, nodes, script string) *run.Run {
	t.Helper()
	realm, cfg := t.TempDir(), t.TempDir()
	project := treetest.Project(t, "repos/app")
	files := map[string]string{
		filepath.Join(realm, "pack.yaml"):  "name: test\nversion: 0.1.0\ngraph: graph.yaml\n" + pack,
		filepath.Join(realm, "graph.yaml"): "name: test\nversion: 1.0.0\nnodes:\n" + nodes,
		filepath.Join(realm, "p.md"):       "Do the work.\n",
		filepath.Join(cfg, "script.yaml"):  script,
		filepath.Join(cfg, "factory.yaml"): "mode: " + mode + "\n" + gates +
			"roles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n" +
			"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := run.Start(context.Background(), run.Options{Project: project, Config: filepath.Join(cfg, "factory.yaml"), Realm: realm})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func ledgerText(t *testing.T, r *run.Run) string {
	t.Helper()
	if _, err := ledger.Verify(filepath.Join(r.Dir, "ledger.jsonl"), r.Ledger.Head()); err != nil {
		t.Fatalf("the ledger doesn't verify: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, "ledger.jsonl"))
	return string(b)
}

const okScript = `steps:
  a:
    - actions: [ { write: { path: out.txt, content: "done\n" } } ]
      result: { status: ok, summary: wrote out.txt, usage: { in: 100, out: 10, cache: 5, turns: 2, cost_usd: 0.5 } }
`

func TestAgentAndExecNodesRunInOrder(t *testing.T) {
	r := start(t, "key-points", "", "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n  - { id: check, kind: exec, run: [grep, -q, done, out.txt], repo: repos/app }\n", okScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	s, _ := run.LoadState(r.Dir)
	if s.Status != run.StatusDone || s.Position != 2 || s.Usage.InTok != 100 || s.Usage.CostUSD != 0.5 {
		t.Errorf("run.json = %+v", s)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"node_finished","ygg.attempt":1,"gen_ai.agent.name":"planner"`,
		`"gen_ai.usage.input_tokens":100`, `"event":"run_finished"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
}

func TestAFailedNodeStopsTheRunAndResumeRerunsIt(t *testing.T) {
	script := "steps:\n  a:\n    - result: { status: failed, summary: tests fail }\n    - result: { status: ok, summary: fixed }\n"
	r := start(t, "key-points", "", "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "tests fail") {
		t.Fatalf("Advance() = %+v, %v; want failed with the agent's summary", out, err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("the second Advance() = %+v, %v; want done", out, err)
	}
	if r.State.Attempts["a"] != 2 {
		t.Errorf("attempts = %d, want 2", r.State.Attempts["a"])
	}
}

func TestAMalformedAgentResultIsAFailureNotAPass(t *testing.T) {
	script := "steps:\n  a:\n    - result: { status: ok, usage: { turns: -1 } }\n"
	r := start(t, "key-points", "", "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed {
		t.Errorf("Advance() = %+v, %v; want failed", out, err)
	}
}

func TestExecGetsNoCredentialsFromTheOperator(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "operator-token")
	t.Setenv("ANTHROPIC_API_KEY", "operator-key")
	r := start(t, "key-points", "", "  - { id: env, kind: exec, run: [sh, -c, \"env > env.txt\"], repo: repos/app }\n", "steps: {}\n")
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(r.RepoDir("repos/app"), "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name, _, _ := strings.Cut(line, "=")
		switch name {
		case "PATH", "HOME", "LANG", "PWD", "SHLVL", "_": // the last three are set by sh itself
		default:
			t.Errorf("the exec node saw %s", name)
		}
	}
}

func TestAnExecTimeoutKillsTheProcessGroup(t *testing.T) {
	r := start(t, "key-points", "",
		"  - { id: slow, kind: exec, run: [sh, -c, \"sleep 30 & echo $! > child.pid; wait\"], timeout: 300ms, repo: repos/app }\n", "steps: {}\n")
	began := time.Now()
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want failed", out, err)
	}
	if time.Since(began) > 10*time.Second {
		t.Errorf("Advance() took %v; the timeout didn't stop it", time.Since(began))
	}
	pid := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(r.RepoDir("repos/app"), "child.pid")))))
	// A killed child is dead once it is gone or a zombie: in a container whose PID 1 doesn't
	// reap orphans, such as CI's, it stays a zombie.
	for deadline := time.Now().Add(2 * time.Second); running(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("the child %s outlived the timeout", pid)
			break
		}
	}
}

// running reports whether process pid exists and isn't a zombie.
func running(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestAnInterruptedAgentCountsItsAttempt(t *testing.T) {
	script := "steps:\n  a:\n    - hang: true\n    - result: { status: ok, summary: done }\n"
	r := start(t, "key-points", "", "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n", script)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want failed after the interrupt", out, err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("resume = %+v, %v; want done", out, err)
	}
	if r.State.Attempts["a"] != 2 {
		t.Errorf("attempts = %d, want 2", r.State.Attempts["a"])
	}
	if !strings.Contains(ledgerText(t, r), "context deadline exceeded") {
		t.Error("the ledger doesn't record why attempt 1 failed")
	}
}

func TestRunJSONRecordsTheRunningExecProcess(t *testing.T) {
	// The node waits up to 5s for the record, since the engine saves it just after the node starts.
	peek := `for i in $(seq 50); do grep -q pgid ../.factory/runs/*/run.json && break; sleep 0.1; done; cat ../../.factory/runs/*/run.json > seen.json`
	r := start(t, "key-points", "", "  - { id: peek, kind: exec, run: [sh, -c, '"+peek+"'], repo: repos/app }\n", "steps: {}\n")
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	seen, err := os.ReadFile(filepath.Join(r.RepoDir("repos/app"), "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seen), `"node": "peek"`) || !strings.Contains(string(seen), `"pgid":`) {
		t.Errorf("run.json didn't record the exec node's process group while it ran:\n%s", seen)
	}
	if s, _ := run.LoadState(r.Dir); s.Exec != nil {
		t.Errorf("run.json still records an exec process after the node ended: %+v", s.Exec)
	}
}

func TestEveryNodeInALeafEndsWithACheckpoint(t *testing.T) {
	r := start(t, "key-points", "", "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n  - { id: check, kind: exec, run: [grep, -q, done, out.txt], repo: repos/app }\n", okScript)
	base := r.State.Leaves["repos/app"].Base
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	head, _ := l.Head(context.Background())
	if cp := r.State.Leaves["repos/app"].Checkpoint; cp != head || cp == base {
		t.Errorf("checkpoint = %s, head %s, base %s; want the agent's commit recorded", cp, head, base)
	}
	if files, _ := l.Git(context.Background(), "show", "--name-only", "--format=%s", head); files != "ygg: run "+r.State.ID+", node a, attempt 1\n\nout.txt" {
		t.Errorf("the checkpoint commit is %q, want the agent's out.txt under a message naming the node", files)
	}
	if st, _ := l.Git(context.Background(), "status", "--porcelain"); st != "" {
		t.Errorf("the leaf has uncommitted work after the run: %q", st)
	}
	if !strings.Contains(ledgerText(t, r), `"ygg.node.id":"a","event":"node_finished"`) || !strings.Contains(ledgerText(t, r), `"ygg.ref.commit":"`+head+`"`) {
		t.Error("node a's node_finished line doesn't carry its checkpoint commit")
	}
}
