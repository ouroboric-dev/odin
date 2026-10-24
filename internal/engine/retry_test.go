package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

const oneNode = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"

// waits is a Sleep that records its waits and returns at once.
type waits struct{ got []time.Duration }

func (w *waits) sleep(_ context.Context, d time.Duration) error { w.got = append(w.got, d); return nil }

func hooks(w *waits) run.Hooks {
	return run.Hooks{Backoff: &jormungandr.Backoff{Base: time.Second, Max: time.Minute, Retries: 3}, Sleep: w.sleep}
}

func TestATransientFailureIsRetriedWithABackoffAndLeavesNoTraceInTheAttempts(t *testing.T) {
	script := `steps:
  a:
    - error: the CLI crashed at start
      kind: transient
    - result: { status: failed, summary: HTTP 429, failure: transient, usage: { in: 5, out: 1, turns: 1 } }
    - actions: [ { write: { path: a.txt, content: "a\n" } } ]
      result: { status: ok, summary: did a, usage: { in: 100, out: 10, turns: 2 } }
`
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done after two retries", out, err)
	}
	if len(w.got) != 2 || w.got[0] != time.Second || w.got[1] != 2*time.Second {
		t.Errorf("waits = %v, want 1s then 2s", w.got)
	}
	text := ledgerText(t, r)
	if strings.Count(text, `"event":"retry"`) != 2 || !strings.Contains(text, "HTTP 429") || strings.Count(text, `"event":"node_failed"`) != 2 || strings.Count(text, `"event":"node_finished"`) != 1 {
		t.Errorf("the ledger should hold two failed tries, two retries and one finish:\n%s", text)
	}
	if u := r.State.Usage; u.InTok != 105 || u.Turns != 3 {
		t.Errorf("usage = %+v; the failed try's tokens count too", u)
	}
}

func TestTransientFailuresThatOutlastTheRetriesFailTheRunAndAResumeTriesAgain(t *testing.T) {
	script := "steps:\n  a:\n    - { error: down, kind: transient }\n    - { error: down, kind: transient }\n    - { error: down, kind: transient }\n    - { error: down, kind: transient }\n    - result: { status: ok, summary: did a }\n"
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "4 times in a row") || len(w.got) != 3 {
		t.Fatalf("Advance() = %+v, %v after %d waits; want a failed run after 3 retries", out, err, len(w.got))
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Errorf("the resume: %+v, %v; want done", out, err)
	}
}

func TestAnAgentFailureIsNotRetried(t *testing.T) {
	script := "steps:\n  a:\n    - { error: tests fail }\n    - result: { status: ok }\n"
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || len(w.got) != 0 || r.State.Attempts["a"] != 1 {
		t.Errorf("Advance() = %+v, %v; %d waits, %d attempts; want one try and a failed run", out, err, len(w.got), r.State.Attempts["a"])
	}
}

func TestAnInfrastructureFailureStopsTheRunWithNoRetry(t *testing.T) {
	script := "steps:\n  a:\n    - { error: podman is gone, kind: infra }\n    - result: { status: ok }\n"
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || len(w.got) != 0 || !strings.Contains(out.Err.Error(), "podman is gone") {
		t.Errorf("Advance() = %+v, %v; %d waits; want a failed run, no retry", out, err, len(w.got))
	}
}

func TestAUsedUpQuotaPausesTheRunAndAResumeStartsTheNodeAgain(t *testing.T) {
	script := `steps:
  a:
    - actions: [ { write: { path: half.txt, content: "half\n" } } ]
      error: out of credits
      kind: quota
      usage: { in: 40, out: 4, turns: 1 }
    - actions: [ { write: { path: a.txt, content: "a\n" } } ]
      result: { status: ok, summary: did a }
`
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusPaused || len(w.got) != 0 {
		t.Fatalf("Advance() = %+v, %v; want a paused run, with no retry", out, err)
	}
	p := r.State.Paused
	if r.State.Status != run.StatusPaused || p == nil || p.Node != "a" || !strings.Contains(p.Reason, "out of credits") {
		t.Errorf("state = %s, pause %+v", r.State.Status, p)
	}
	text := ledgerText(t, r)
	if !strings.Contains(text, `"event":"run_paused"`) || !strings.Contains(text, "out of credits") {
		t.Errorf("the ledger doesn't say why the run paused:\n%s", text)
	}
	if r.State.Usage.InTok != 40 {
		t.Errorf("usage = %+v; the session that hit the quota still spent tokens", r.State.Usage)
	}
	// The resume opens the run from its files, which puts the leaf back to the checkpoint.
	r = reopen(t, r)
	if _, err := Advance(context.Background(), r); err != nil || r.State.Status != run.StatusDone || r.State.Paused != nil {
		t.Fatalf("the resume left the run %s, pause %+v, %v", r.State.Status, r.State.Paused, err)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "half.txt")); err == nil {
		t.Error("what the paused session wrote is still in the leaf; the node must start from the checkpoint")
	}
}
