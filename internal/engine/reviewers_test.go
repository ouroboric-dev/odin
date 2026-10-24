package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// reviewLoop is a loop with the check unit and one reviewer, spec, who runs as the scripted planner.
const reviewLoop = `  - id: build
    kind: loop
    input: [rejection, refusal]
    output: ygg.implement/v1
    repo: repos/app
    role: planner
    prompt: p.md
    attempts: 3
    checks: [unit]
    reviewers:
      - { id: spec, role: planner, prompt: p.md, output: ygg.review/v1 }
`

func startReview(t *testing.T, mode, script string) *run.Run {
	t.Helper()
	return startWith(t, setup{mode: mode, pack: unitCheck, nodes: reviewLoop, script: script})
}

const majorFinding = `{ id: spec-1, source: spec, severity: major, location: "build.txt:1", claim: "build.txt doesn't say what the brief asks", status: open }`

func fixedFinding(proof string) string {
	return `{ id: spec-1, source: spec, severity: major, location: "build.txt:1", claim: "build.txt doesn't say what the brief asks", status: fixed, proof: "` + proof + `" }`
}

func TestAReviewerSeesItsFindingsAndClosesThemOnlyWithProof(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done v1") + " ]\n      result: { status: ok }\n" +
		"    - actions: [ " + write("build.txt", "done v2") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - result: { status: ok, report: [ " + majorFinding + " ] }\n" +
		"    - result: { status: ok, report: [ " + fixedFinding("build.txt:1 now says what the brief asks; check unit passed") + " ] }\n"
	r := startReview(t, "key-points", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Loops["build"].State.Attempt != 2 || r.State.Attempts["build-spec"] != 2 {
		t.Errorf("loop attempt %d, reviewer sessions %d; want 2 and 2", r.State.Loops["build"].State.Attempt, r.State.Attempts["build-spec"])
	}
	first, second := transcript(t, r, "build-spec-1"), transcript(t, r, "build-spec-2")
	if !strings.Contains(first, "Your id is spec") || !strings.Contains(first, "The checks that passed on this work: unit.") || strings.Contains(first, "reported before") {
		t.Errorf("the first review was told:\n%s", first)
	}
	if !strings.Contains(second, "Findings you reported before") || !strings.Contains(second, `\"id\": \"spec-1\"`) {
		t.Errorf("the second review wasn't shown its finding:\n%s", second)
	}
	if !strings.Contains(transcript(t, r, "build-2"), "- spec-1 (major, from spec) at build.txt:1") {
		t.Error("the second implementer attempt wasn't given the reviewer's finding")
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"finding_raised"`, `"ygg.finding.id":"spec-1"`, `"ygg.finding.source":"spec"`, `"event":"finding_closed"`, `"ygg.finding.attempts":2`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	// The implementer is never told it may close a finding; only the reviewer did.
	if got := r.State.Loops["build"].State.Book.Records["spec-1"]; got == nil || got.Status != gungnir.FindingFixed || !strings.Contains(got.Proof, "check unit passed") {
		t.Errorf("record = %+v", got)
	}
}

func TestReviewersRunReadOnlyAndTheImplementerDoesNot(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if !strings.Contains(transcript(t, r, "build-spec-1"), `"read_only": true`) || !strings.Contains(transcript(t, r, "build-1"), `"read_only": false`) {
		t.Error("the reviewer must run read-only, and the implementer not")
	}
}

// Backlog B-018: an ignored file is a change too.
func TestAReviewerThatWritesAnIgnoredFileIsAHardStop(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write(".gitignore", "*.log") + ", " + write("build.txt", "done") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - actions: [ " + write("notes.log", "I was here") + " ]\n      result: { status: ok, report: [] }\n"
	r := startReview(t, "unattended", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "notes.log")); err != nil {
		t.Error("the leaf was cleaned; a quarantine keeps what the reader wrote, for investigation")
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	if !strings.Contains(string(gate), "the read-only role planner changed the leaf") || !strings.Contains(string(gate), "+ !! notes.log") {
		t.Errorf("the gate file = %s", gate)
	}
}

func TestIgnoredFilesThatWereThereBeforeAreNotTheReadersDoing(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write(".gitignore", "*.log") + ", " + write("build.txt", "done") + ", " + write("cache.log", "left by the build") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; an ignored file the build left is not the reviewer's", out, err)
	}
}

// Backlog B-018, for a read-only agent node.
func TestAReadOnlyRoleThatWritesAnIgnoredFileIsAHardStop(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n  - { id: look, kind: agent, role: planner, prompt: p.md, repo: repos/app, read_only: true, input: [rejection], output: ygg.implement/v1 }\n"
	script := "steps:\n  a:\n    - actions: [ " + write(".gitignore", "*.log") + " ]\n      result: { status: ok }\n  look:\n    - actions: [ " + write("notes.log", "I was here") + " ]\n      result: { status: ok }\n"
	r := start(t, "key-points", "", nodes, script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Errorf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
}

// Review I1: a read-only session that changed the leaf and then failed is checked all the same.
func TestAReviewerThatWritesAnIgnoredFileAndFailsAsAnAgentIsAHardStop(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write(".gitignore", "*.log") + ", " + write("build.txt", "done") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - actions: [ " + write("notes.log", "I was here") + " ]\n      error: the session timed out\n" +
		"    - result: { status: ok, report: [] }\n"
	r := startReview(t, "unattended", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if r.State.Attempts["build-spec"] != 1 {
		t.Errorf("reviewer sessions %d; the reviewer must not be asked again over a changed leaf", r.State.Attempts["build-spec"])
	}
}

func TestAReadOnlyRoleThatWritesAnIgnoredFileAndFailsIsAHardStop(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n  - { id: look, kind: agent, role: planner, prompt: p.md, repo: repos/app, read_only: true, input: [rejection], output: ygg.implement/v1 }\n"
	script := "steps:\n  a:\n    - actions: [ " + write(".gitignore", "*.log") + " ]\n      result: { status: ok }\n" +
		"  look:\n    - actions: [ " + write("notes.log", "I was here") + " ]\n      error: the session timed out\n"
	r := start(t, "key-points", "", nodes, script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Errorf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
}

// A retry resets the leaf: a read-only session's leaf is compared before that, or the reset hides what it wrote.
func TestAReviewerThatWritesAFileAndFailsTransientlyIsAHardStop(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - actions: [ " + write("notes.txt", "I was here") + " ]\n      error: HTTP 529 overloaded\n      kind: transient\n" +
		"    - result: { status: ok, report: [] }\n"
	w := &waits{}
	r := startWith(t, setup{mode: "unattended", pack: unitCheck, nodes: reviewLoop, script: script, hooks: hooks(w)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "notes.txt")); err != nil {
		t.Error("the leaf was reset; a quarantine keeps what the reader wrote, for investigation")
	}
}
