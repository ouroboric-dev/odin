package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// wantHeading is in the decisions input of a prompt once an approval gave a note.
const wantHeading = "### decisions"

// decided is how the decisions input renders the note of gate: one decision, note-<gate>, given at gate.
func decided(gate, note string) string {
	return "- note-" + gate + " (at " + gate + "): " + note + "\n"
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
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "Use the warm palette.")); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	if err := Approve(bg, r, "spec-gate", Note(r, "spec-gate", "Albums are private by default.")); err != nil {
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
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "Warm.")); err != nil {
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
    input: [decisions, rejection, refusal, open_findings, check_results]
    output: ygg.implement/v1
    repo: repos/app
    role: planner
    prompt: p.md
    attempts: 3
    checks: [unit]
    reviewers:
      - { id: spec, role: planner, prompt: p.md, input: [decisions, change, open_findings, check_results], output: ygg.review/v1 }
      - { id: arch, role: planner, prompt: p.md, input: [decisions, change, open_findings, check_results], output: ygg.review/v1 }
`

func TestNoteReachesLoopSessions(t *testing.T) {
	script := "steps:\n  design:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n" +
		"  build-arch:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "step-by-step", pack: unitCheck, nodes: decisionLoop, script: script})
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "Warm.")); err != nil {
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
		if err := Approve(bg, r, "design-gate", Note(r, "design-gate", note)); err != nil {
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
	if err := Approve(bg, r, "build", Note(r, "build", "ship it as it is")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(r.State.Decisions) != 0 || strings.Contains(sessionPrompt(t, r, "after-1"), "ship it as it is") {
		t.Errorf("an escalation's note is a decision: %+v", r.State.Decisions)
	}
	if !strings.Contains(ledgerText(t, r), `"ygg.note":"{\"decisions\":[{\"id\":\"note-build\",\"text\":\"ship it as it is\"}]}"`) {
		t.Error("the gate_approved event lacks the note")
	}

	// policy-violation.
	pack := "checks:\n  unit: [sh, -c, \"mkdir -p vendor/x/.git && grep -q done build.txt\"]\n"
	pv := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r = startWith(t, setup{mode: "unattended", pack: pack, nodes: loopNode, script: pv})
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s", out, err, r.State.PendingGate)
	}
	if err := Approve(bg, r, "build", Note(r, "build", "fine")); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 0 || !strings.Contains(ledgerText(t, r), `\"text\":\"fine\"`) {
		t.Errorf("policy-violation: decisions %+v", r.State.Decisions)
	}

	// shrink-guard.
	r = shrunkRun(t)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Approve(bg, r, "move", Note(r, "move", "shrank on purpose")); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 0 || !strings.Contains(ledgerText(t, r), `\"text\":\"shrank on purpose\"`) {
		t.Errorf("shrink-guard: decisions %+v", r.State.Decisions)
	}
}

// A gate met again (a back_to return past it, or a rejection there and then an approval) adds what it is given to
// what it was given before, and moves to the end, the order in which they were given. A decision already kept, at any
// gate, is not kept twice (Approve refuses one with another text, ruling R46(3)); an answer to a question answered
// before replaces it. Nothing given restates nothing, and the earlier decisions hold.
func TestAGateMetAgainAddsToWhatItWasGiven(t *testing.T) {
	s := &run.State{}
	one := func(id, text string) schema.Decisions {
		return schema.Decisions{Decisions: []schema.Decision{{ID: id, Text: text}}}
	}
	keepDecisions(s, "design-gate", one("palette", "Warm."))
	keepDecisions(s, "spec-gate", one("users", "Two users."))
	keepDecisions(s, "spec-gate", one("palette", "Warm."))
	want := []run.GateDecisions{{Gate: "design-gate", Decisions: one("palette", "Warm.")}, {Gate: "spec-gate", Decisions: one("users", "Two users.")}}
	if !reflect.DeepEqual(s.Decisions, want) {
		t.Errorf("a decision given again at another gate: decisions = %+v, want %+v", s.Decisions, want)
	}
	keepDecisions(s, "design-gate", schema.Decisions{Decisions: []schema.Decision{{ID: "palette", Text: "Warm."}, {ID: "font", Text: "Serif."}},
		Answers: []schema.Answer{{Question: "q1", Text: "Yes."}}})
	keepDecisions(s, "design-gate", schema.Decisions{Answers: []schema.Answer{{Question: "q1", Text: "No."}}})
	want = []run.GateDecisions{{Gate: "spec-gate", Decisions: one("users", "Two users.")},
		{Gate: "design-gate", Decisions: schema.Decisions{Decisions: []schema.Decision{{ID: "palette", Text: "Warm."}, {ID: "font", Text: "Serif."}},
			Answers: []schema.Answer{{Question: "q1", Text: "No."}}}}}
	if !reflect.DeepEqual(s.Decisions, want) {
		t.Errorf("decisions = %+v, want %+v", s.Decisions, want)
	}
	keepDecisions(s, "design-gate", schema.Decisions{})
	keepDecisions(s, "spec-gate", one("users", "Two users."))
	if !reflect.DeepEqual(s.Decisions, want) {
		t.Errorf("after approvals that gave nothing new: decisions = %+v, want %+v", s.Decisions, want)
	}
	keepDecisions(s, "plan-gate", schema.Decisions{Reason: "Thin."})
	if !reflect.DeepEqual(s.Decisions, want) {
		t.Errorf("a rejection's reason alone was kept as a decision: %+v", s.Decisions)
	}
}

// A decision or an answer reads as its text in the decisions input: a fence it opens is closed in its item, and no
// line of it starts an HTML block or a heading (review focus 4).
func TestADecisionCannotBreakTheDecisionsInput(t *testing.T) {
	r := start(t, "key-points", "", ownerNodes, ownerScript(specCiting("fenced", "html")))
	advanceTo(t, r, "design-gate")
	given := schema.Decisions{Decisions: []schema.Decision{{ID: "fenced", Text: "```\nan open fence"}, {ID: "html", Text: "Plain.\n<!-- never closed\n\n## after"}},
		Answers: []schema.Answer{{Question: "q1", Text: "Yes.\n<div>\n# not a heading"}}}
	if err := Approve(bg, r, "design-gate", given); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	p := promptOf(t, r, "spec-1")
	i, j := strings.Index(p, "## Input: decisions\n"), strings.Index(p, "## Input: rejection\n")
	if i < 0 || j < i {
		t.Fatalf("the prompt lacks its decisions and rejection sections:\n%s", p)
	}
	section := p[i:j]
	want := "### decisions\n\n- fenced (at design-gate):\n  ```\n  an open fence\n  ```\n" +
		"- html (at design-gate): Plain.\n      <!-- never closed\n\n      ## after\n\n" +
		"### answers\n\n- q1 (at design-gate): Yes.\n      <div>\n      # not a heading\n  The question: May guests download photos?\n"
	if !strings.Contains(section, want) {
		t.Errorf("the decisions input is not as its fields:\nwant %q\nin:\n%s", want, section)
	}
	headings, html := parsedMarkdown([]byte(section))
	if html != 0 || !slices.Equal(headings, []string{"## Input: decisions", "### decisions", "### answers"}) {
		t.Errorf("goldmark reads %d HTML block(s) and the headings %q; want none, and Odin's own", html, headings)
	}
}

func TestRejectionDropsTheNotesOfGatesItGoesBackPast(t *testing.T) {
	// spec-gate reruns design, which is before design-gate: the run meets design-gate again.
	back := strings.Replace(decisionNodes, "gate: spec, reruns: spec }", "gate: spec, reruns: design }", 1)
	r := start(t, "step-by-step", "", back, decisionScript)
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "Warm.")); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	if len(r.State.Decisions) != 1 {
		t.Fatalf("decisions = %+v; want the design gate's note", r.State.Decisions)
	}
	if err := Reject(bg, r, "spec-gate", Reason("Start from the design.")); err != nil {
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
	Approve(bg, r, "design-gate", Note(r, "design-gate", "Warm."))
	advanceTo(t, r, "spec-gate")
	if err := Reject(bg, r, "spec-gate", Reason("Name the users.")); err != nil {
		t.Fatal(err)
	}
	if len(r.State.Decisions) != 1 || !reflect.DeepEqual(r.State.Decisions[0], run.GateDecisions{Gate: "design-gate", Decisions: Note(r, "design-gate", "Warm.")}) {
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
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "Warm.")); err != nil {
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

// parsedMarkdown is what goldmark, a CommonMark parser, reads in a text: its headings, as "## text", and the number of
// HTML blocks.
func parsedMarkdown(body []byte) (headings []string, html int) {
	doc := goldmark.New().Parser().Parse(gmtext.NewReader(body))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch h := n.(type) {
		case *ast.Heading:
			var words []string
			for i := 0; i < h.Lines().Len(); i++ {
				seg := h.Lines().At(i)
				words = append(words, string(seg.Value(body)))
			}
			headings = append(headings, strings.Repeat("#", h.Level)+" "+strings.TrimSpace(strings.Join(words, " ")))
		case *ast.HTMLBlock:
			html++
		}
		return ast.WalkContinue, nil
	})
	return headings, html
}

// Ruling R47: a --note takes the next free id, note-<gate>, then note-<gate>-2 and on, so a gate met again after a
// back_to return past it can take a new note. Both are kept, and the spec cites each by its id.
func TestANoteAtAGateMetAgainTakesTheNextFreeId(t *testing.T) {
	nodes := "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [decisions, rejection, refusal], output: ygg.plan/v1 }\n" +
		"  - { id: plan-gate, kind: gate, gate: design, reruns: plan }\n" +
		"  - { id: ready, kind: tasks, from: plan, back_to: plan }\n" +
		"  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [decisions, rejection, refusal], output: ygg.spec/v1 }\n"
	bad := planOf(taskOf("p-T001", "repos/app", "L"))
	good := planOf(taskOf("p-T001", "repos/app", "S"))
	script := planScript(bad, good) + "  spec:\n    - result: { status: ok, summary: wrote the spec, output: '" + specCiting("note-plan-gate", "note-plan-gate-2") + "' }\n"
	r := start(t, "key-points", "", nodes, script)
	advanceTo(t, r, "plan-gate")
	if err := Approve(bg, r, "plan-gate", Note(r, "plan-gate", "Warm.")); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "plan-gate") // the plan was not ready: back to plan, and the gate is met again
	if got := Note(r, "plan-gate", "Warm."); got.Decisions[0].ID != "note-plan-gate" {
		t.Errorf("the same note again takes the id %s; want note-plan-gate, which it is", got.Decisions[0].ID)
	}
	if err := Approve(bg, r, "plan-gate", Note(r, "plan-gate", "Cold.")); err != nil {
		t.Fatalf("Approve() with a new note at a gate met again = %v", err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done: the spec cites both notes", out, err)
	}
	want := []run.GateDecisions{{Gate: "plan-gate", Decisions: schema.Decisions{Decisions: []schema.Decision{
		{ID: "note-plan-gate", Text: "Warm."}, {ID: "note-plan-gate-2", Text: "Cold."}}}}}
	if !reflect.DeepEqual(r.State.Decisions, want) {
		t.Errorf("decisions = %+v; want both notes, each under its own id", r.State.Decisions)
	}
	if p := promptOf(t, r, "spec-1"); !strings.Contains(p, "- note-plan-gate (at plan-gate): Warm.\n- note-plan-gate-2 (at plan-gate): Cold.\n") {
		t.Errorf("the spec's decisions input lacks both notes with their ids:\n%s", p)
	}
	if r.State.Attempts["spec"] != 1 {
		t.Errorf("spec ran %d time(s); want 1: it cites both ids", r.State.Attempts["spec"])
	}
}
