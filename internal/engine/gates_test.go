package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

const gateNodes = "  - { id: spec, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n  - { id: check, kind: exec, run: [test, -s, spec.md], repo: repos/app }\n  - { id: spec-gate, kind: gate, gate: spec, reruns: spec }\n"

const gateScript = `steps:
  spec:
    - actions: [ { write: { path: spec.md, content: "v1\n" } } ]
      result: { status: ok, summary: wrote spec v1, output: '{"summary": "done"}' }
    - actions: [ { write: { path: spec.md, content: "v2\n" } } ]
      result: { status: ok, summary: wrote spec v2, output: '{"summary": "done"}' }
`

func TestAStopWritesAGateFileAndWaits(t *testing.T) {
	r := start(t, "step-by-step", "", gateNodes, gateScript)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
		t.Fatalf("Advance() = %+v, %v; want stopped at spec-gate", out, err)
	}
	s, _ := run.LoadState(r.Dir)
	if s.Status != run.StatusStopped || s.Pending != "spec-gate" {
		t.Errorf("run.json = %s, pending %q", s.Status, s.Pending)
	}
	gf, err := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stopped for a decision", "spec (agent, attempt 1): wrote spec v1",
		"check (exec, attempt 1): exit 0", "ygg approve " + s.ID + " spec-gate", "ygg reject " + s.ID + " spec-gate"} {
		if !strings.Contains(string(gf), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gf)
		}
	}
	if !strings.Contains(ledgerText(t, r), `"event":"gate_pending"`) {
		t.Error("no gate_pending in the ledger")
	}
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Error("Advance() on a waiting run moved on without a decision")
	}
}

func TestAnAutoPassStillWritesTheGateFile(t *testing.T) {
	r := start(t, "key-points", "", gateNodes, gateScript)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done (spec passes in key-points)", out, err)
	}
	gf, err := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
	if err != nil || !strings.Contains(string(gf), "passed automatically") {
		t.Errorf("gate file = %q, %v", gf, err)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"gate_auto_passed"`) {
		t.Error("no gate_auto_passed in the ledger")
	}
}

func TestApproveMovesPastTheGate(t *testing.T) {
	r := start(t, "step-by-step", "", gateNodes, gateScript)
	Advance(context.Background(), r)
	if err := Approve(context.Background(), r, "design-gate", ""); err == nil {
		t.Error("Approve() of a gate that isn't waiting succeeded")
	}
	if err := Approve(context.Background(), r, "spec-gate", "looks right"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"gate_approved","ygg.gate.name":"spec-gate","ygg.note":"looks right"`) {
		t.Error("the ledger doesn't record the approval with its note")
	}
}

func TestRejectRerunsTheNodeWithTheReason(t *testing.T) {
	r := start(t, "step-by-step", "", gateNodes, gateScript)
	Advance(context.Background(), r)
	if err := Reject(context.Background(), r, "spec-gate", " "); err == nil {
		t.Error("Reject() without a reason succeeded")
	}
	if err := Reject(context.Background(), r, "spec-gate", "Name the users."); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
		t.Fatalf("Advance() = %+v, %v; want stopped at spec-gate again", out, err)
	}
	var tr struct{ Prompt string }
	b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", "spec-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &tr)
	if !strings.Contains(tr.Prompt, "Name the users.") {
		t.Errorf("attempt 2's prompt lacks the reason:\n%s", tr.Prompt)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "gates", "spec-gate-2.md")); err != nil {
		t.Errorf("the second stop has no gate file of its own: %v", err)
	}
	if r.State.Reasons["spec"] != "" {
		t.Error("the reason is kept after the node used it")
	}
}

// Review I2: new work under a loop that passed goes through the loop again.
func TestARejectionThatRerunsANodeBeforeAPassedLoopRunsTheLoopAgain(t *testing.T) {
	nodes := "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection, refusal], output: ygg.plan/v1 }\n" + loopNode +
		"  - { id: review, kind: gate, gate: spec, reruns: plan }\n"
	script := "steps:\n  plan:\n    - actions: [ " + write("plan.md", "v1") + " ]\n      result: { status: ok, output: '" + planDone + "' }\n" +
		"    - actions: [ " + write("plan.md", "v2") + " ]\n      result: { status: ok, output: '" + planDone + "' }\n" +
		"  build:\n    - actions: [ " + write("build.txt", "done v1") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done v2") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startPack(t, "step-by-step", "", unitCheck, nodes, script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || out.Gate != "review" {
		t.Fatalf("Advance() = %+v, %v; want stopped at review", out, err)
	}
	if err := Reject(context.Background(), r, "review", "Plan for two users."); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || out.Gate != "review" {
		t.Fatalf("Advance() = %+v, %v; want stopped at review again", out, err)
	}
	if r.State.Attempts["plan"] != 2 || r.State.Attempts["build"] != 2 {
		t.Errorf("plan sessions %d, build sessions %d; want 2 and 2: the loop checks the new work", r.State.Attempts["plan"], r.State.Attempts["build"])
	}
}
