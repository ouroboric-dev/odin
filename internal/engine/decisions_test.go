package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// wantHeading is in the decisions input of a prompt once an approval gave a note.
const wantHeading = "### decisions"

// decided is how the decisions input renders the note of gate.
func decided(gate, note string) string {
	return "    \"id\": \"" + gate + "\",\n    \"text\": \"" + note + "\""
}

// promptOf is the prompt a session was given, from its transcript (key-attempt, as "spec-1").
func sessionPrompt(t *testing.T, r *run.Run, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr struct{ Prompt string }
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Prompt
}

// A design stage, a spec stage with a conformance check, and a plan: three kinds of prompt (an agent in a
// leaf, a scratch agent) after two gates.
const decisionNodes = "  - { id: design, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [decisions, rejection], output: ygg.implement/v1 }\n" +
	"  - { id: design-gate, kind: gate, gate: spec, reruns: design }\n" +
	"  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [decisions, rejection], output: ygg.implement/v1 }\n" +
	"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec, input: [decisions, rejection], output: ygg.conformance/v1 }\n" +
	"  - { id: spec-gate, kind: gate, gate: spec, reruns: spec }\n" +
	"  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [decisions, rejection, refusal], output: ygg.plan/v1 }\n"

const decisionScript = `steps:
  design:
    - actions: [ { write: { path: design.md, content: "d1\n" } } ]
      result: { status: ok, summary: designed, output: '{"summary": "done"}' }
    - actions: [ { write: { path: design.md, content: "d2\n" } } ]
      result: { status: ok, summary: designed again, output: '{"summary": "done"}' }
  spec:
    - actions: [ { write: { path: out/specs/spec.md, content: "# Spec\n" } } ]
      result: { status: ok, summary: wrote the spec, output: '{"summary": "done"}' }
    - actions: [ { write: { path: out/specs/spec.md, content: "# Spec 2\n" } } ]
      result: { status: ok, summary: wrote the spec again, output: '{"summary": "done"}' }
  conformance:
    - result: { status: ok, summary: checked, output: '{"deviation": false}' }
    - result: { status: ok, summary: checked, output: '{"deviation": false}' }
  plan:
    - actions: [ { write: { path: plan.md, content: "p\n" } } ]
      result: { status: ok, summary: planned, output: '` + planDone + `' }
`

func advanceTo(t *testing.T, r *run.Run, gate string) {
	t.Helper()
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != gate {
		t.Fatalf("Advance() = %+v, %v; want stopped at %s", out, err, gate)
	}
}

func TestApprovalNoteReachesLaterStages(t *testing.T) {
	r := start(t, "step-by-step", "", decisionNodes, decisionScript)
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", "Use the warm palette."); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	if err := Approve(bg, r, "spec-gate", "Albums are private by default."); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if p := sessionPrompt(t, r, "design-1"); strings.Contains(p, wantHeading) {
		t.Errorf("design's prompt, from before the approval, holds the block:\n%s", p)
	}
	first := decided("design-gate", "Use the warm palette.")
	second := decided("spec-gate", "Albums are private by default.")
	for _, node := range []string{"spec-1", "conformance-1"} {
		p := sessionPrompt(t, r, node)
		if !strings.Contains(p, wantHeading) || !strings.Contains(p, first) || strings.Contains(p, second) {
			t.Errorf("%s's prompt must hold the first note only:\n%s", node, p)
		}
	}
	p := sessionPrompt(t, r, "plan-1")
	i, j := strings.Index(p, first), strings.Index(p, second)
	if !strings.Contains(p, wantHeading) || i < 0 || j < i {
		t.Errorf("plan's prompt must hold both notes, the first before the second:\n%s", p)
	}
	if !strings.Contains(p, "the decision wins") {
		t.Errorf("plan's prompt lacks the block's text:\n%s", p)
	}
}

func TestBlockComesAfterTheBrief(t *testing.T) {
	r := start(t, "step-by-step", "", strings.ReplaceAll(decisionNodes, "input: [decisions,", "input: [brief, decisions,"), decisionScript)
	r.Setup.BriefText = "Build a gallery."
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", "Warm."); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	for _, node := range []string{"spec-1", "conformance-1"} {
		p := sessionPrompt(t, r, node)
		if b, d := strings.Index(p, "Build a gallery."), strings.Index(p, wantHeading); b < 0 || d < b {
			t.Errorf("%s: the block must follow the brief:\n%s", node, p)
		}
	}
}

const decisionLoop = `  - { id: design, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [decisions, rejection], output: ygg.implement/v1 }
  - { id: design-gate, kind: gate, gate: spec, reruns: design }
  - id: build
    kind: loop
    input: [decisions, rejection, refusal]
    output: ygg.implement/v1
    repo: repos/app
    role: planner
    prompt: p.md
    attempts: 3
    checks: [unit]
    reviewers:
      - { id: spec, role: planner, prompt: p.md, input: [decisions], output: ygg.review/v1 }
      - { id: arch, role: planner, prompt: p.md, input: [decisions], output: ygg.review/v1 }
`

func TestNoteReachesLoopSessions(t *testing.T) {
	script := "steps:\n  design:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n" +
		"  build-arch:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "step-by-step", pack: unitCheck, nodes: decisionLoop, script: script})
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", "Warm."); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	for _, node := range []string{"build-1", "build-spec-1", "build-arch-1"} {
		if p := sessionPrompt(t, r, node); !strings.Contains(p, wantHeading) || !strings.Contains(p, decided("design-gate", "Warm.")) {
			t.Errorf("%s's prompt lacks the note:\n%s", node, p)
		}
	}
}

func TestApprovalWithoutANoteAddsNothing(t *testing.T) {
	for _, note := range []string{"", "   "} {
		r := start(t, "step-by-step", "", decisionNodes, decisionScript)
		advanceTo(t, r, "design-gate")
		if err := Approve(bg, r, "design-gate", note); err != nil {
			t.Fatal(err)
		}
		advanceTo(t, r, "spec-gate")
		if len(r.State.Decisions) != 0 {
			t.Errorf("note %q kept: %+v", note, r.State.Decisions)
		}
		if p := sessionPrompt(t, r, "spec-1"); strings.Contains(p, wantHeading) {
			t.Errorf("note %q added a block:\n%s", note, p)
		}
	}
}

func TestOnlyAGateNoteIsADecision(t *testing.T) {
	// A loop that gave up.
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  after:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	nodes := loopNode + "  - { id: after, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	r := startWith(t, setup{mode: "key-points", pack: unitCheck, nodes: nodes, script: script})
	advanceTo(t, r, "build")
	if r.State.PendingGate != gungnir.GateEscalation {
		t.Fatalf("pending gate %s; want escalation", r.State.PendingGate)
	}
	if err := Approve(bg, r, "build", "ship it as it is"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(r.State.Decisions) != 0 || strings.Contains(sessionPrompt(t, r, "after-1"), "ship it as it is") {
		t.Errorf("an escalation's note is a decision: %+v", r.State.Decisions)
	}
	if !strings.Contains(ledgerText(t, r), `"ygg.note":"ship it as it is"`) {
		t.Error("the gate_approved event lacks the note")
	}

	// policy-violation.
	pack := "checks:\n  unit: [sh, -c, \"mkdir -p vendor/x/.git && grep -q done build.txt\"]\n"
	pv := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r = startWith(t, setup{mode: "unattended", pack: pack, nodes: loopNode, script: pv})
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s", out, err, r.State.PendingGate)
	}
	if err := Approve(bg, r, "build", "fine"); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 0 || !strings.Contains(ledgerText(t, r), `"ygg.note":"fine"`) {
		t.Errorf("policy-violation: decisions %+v", r.State.Decisions)
	}

	// shrink-guard.
	r = shrunkRun(t)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Approve(bg, r, "move", "shrank on purpose"); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 0 || !strings.Contains(ledgerText(t, r), `"ygg.note":"shrank on purpose"`) {
		t.Errorf("shrink-guard: decisions %+v", r.State.Decisions)
	}
}

func TestANoteIsOneLine(t *testing.T) {
	r := start(t, "step-by-step", "", decisionNodes, decisionScript)
	advanceTo(t, r, "design-gate")
	for _, note := range []string{"a\nb", "a\rb", "a\tb", "a\x00b", "a\x1bb", "a\x7fb", "a\u0085b", "a\u2028b", "a\u2029b", "line\n## Heading"} {
		err := Approve(bg, r, "design-gate", note)
		if err == nil || !strings.Contains(err.Error(), "control character") {
			t.Errorf("Approve(%q) = %v; want a refusal that names the control character", note, err)
		}
	}
	if r.State.Pending != "design-gate" || len(r.State.Decisions) != 0 || strings.Contains(ledgerText(t, r), "gate_approved") {
		t.Errorf("a refused note recorded something: pending %q, %+v", r.State.Pending, r.State.Decisions)
	}
	if err := Approve(bg, r, "design-gate", "one line, with ünïcode"); err != nil {
		t.Errorf("a plain note was refused: %v", err)
	}
}

// A back_to return past a gate (land or move back to implement) meets the gate again: its second approval
// replaces the first one's note, so no stale or second note is left. One with no note restates nothing,
// and the earlier decision still holds.
func TestASecondApprovalOfAGateReplacesItsNote(t *testing.T) {
	s := &run.State{}
	keepDecision(s, "design-gate", "Warm.")
	keepDecision(s, "spec-gate", "Two users.")
	keepDecision(s, "design-gate", "Cold.")
	want := []run.Decision{{Gate: "spec-gate", Note: "Two users."}, {Gate: "design-gate", Note: "Cold."}}
	if !slices.Equal(s.Decisions, want) {
		t.Errorf("decisions = %+v, want %+v", s.Decisions, want)
	}
	keepDecision(s, "design-gate", " ")
	if !slices.Equal(s.Decisions, want) {
		t.Errorf("after an approval with no note: decisions = %+v, want %+v", s.Decisions, want)
	}
}

func TestRejectionDropsTheNotesOfGatesItGoesBackPast(t *testing.T) {
	// spec-gate reruns design, which is before design-gate: the run meets design-gate again.
	back := strings.Replace(decisionNodes, "gate: spec, reruns: spec }", "gate: spec, reruns: design }", 1)
	r := start(t, "step-by-step", "", back, decisionScript)
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", "Warm."); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	if len(r.State.Decisions) != 1 {
		t.Fatalf("decisions = %+v; want the design gate's note", r.State.Decisions)
	}
	if err := Reject(bg, r, "spec-gate", "Start from the design."); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 0 {
		t.Errorf("decisions = %+v; a rejection past design-gate drops its note", r.State.Decisions)
	}
	advanceTo(t, r, "design-gate")
	if p := sessionPrompt(t, r, "design-2"); strings.Contains(p, wantHeading) {
		t.Errorf("the rerun of design holds a dropped note:\n%s", p)
	}

	// A rejection back to a node after every gate keeps them all.
	r = start(t, "step-by-step", "", decisionNodes, decisionScript)
	advanceTo(t, r, "design-gate")
	Approve(bg, r, "design-gate", "Warm.")
	advanceTo(t, r, "spec-gate")
	if err := Reject(bg, r, "spec-gate", "Name the users."); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 1 || r.State.Decisions[0] != (run.Decision{Gate: "design-gate", Note: "Warm."}) {
		t.Errorf("decisions = %+v; want the design gate's note kept", r.State.Decisions)
	}
	advanceTo(t, r, "spec-gate")
	if p := sessionPrompt(t, r, "spec-2"); !strings.Contains(p, decided("design-gate", "Warm.")) {
		t.Errorf("the rerun of spec lacks the kept note:\n%s", p)
	}
}

func TestDecisionsSurviveAResume(t *testing.T) {
	r := start(t, "step-by-step", "", decisionNodes, decisionScript)
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", "Warm."); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "run.json"))
	if err != nil || !strings.Contains(string(b), "Warm.") {
		t.Fatalf("run.json lacks the note: %v\n%s", err, b)
	}
	r = reopen(t, r)
	if len(r.State.Decisions) != 1 {
		t.Fatalf("decisions after the resume = %+v", r.State.Decisions)
	}
	advanceTo(t, r, "spec-gate")
	if p := sessionPrompt(t, r, "spec-1"); !strings.Contains(p, decided("design-gate", "Warm.")) {
		t.Errorf("spec's prompt after the resume lacks the note:\n%s", p)
	}
}
