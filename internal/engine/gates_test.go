package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
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
		"check (exec, attempt 1): exit 0", "ygg approve " + s.ID + ` spec-gate [--note "..." | --decisions <file>]`,
		"ygg reject " + s.ID + ` spec-gate --reason "..." | --decisions <file>`} {
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
	if err := Approve(context.Background(), r, "design-gate", Note(r, "design-gate", "")); err == nil {
		t.Error("Approve() of a gate that isn't waiting succeeded")
	}
	if err := Approve(context.Background(), r, "spec-gate", Note(r, "spec-gate", "looks right")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"gate_approved","ygg.gate.name":"spec-gate","ygg.note":"{\"decisions\":[{\"id\":\"note-spec-gate\",\"text\":\"looks right\"}]}"`) {
		t.Error("the ledger doesn't record the approval with its note, as a decisions file")
	}
}

func TestRejectRerunsTheNodeWithTheReason(t *testing.T) {
	r := start(t, "step-by-step", "", gateNodes, gateScript)
	Advance(context.Background(), r)
	if err := Reject(context.Background(), r, "spec-gate", Reason(" ")); err == nil {
		t.Error("Reject() without a reason succeeded")
	}
	if err := Reject(context.Background(), r, "spec-gate", Reason("Name the users.")); err != nil {
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
	if err := Reject(context.Background(), r, "review", Reason("Plan for two users.")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || out.Gate != "review" {
		t.Fatalf("Advance() = %+v, %v; want stopped at review again", out, err)
	}
	if r.State.Attempts["plan"] != 2 || r.State.Attempts["build"] != 2 {
		t.Errorf("plan sessions %d, build sessions %d; want 2 and 2: the loop checks the new work", r.State.Attempts["plan"], r.State.Attempts["build"])
	}
}

// The design gate lists the design's open questions, and the plan gate shows the reuse review's verdict and its
// items, so a person reads them where they decide; a field reads as its text there too.
func TestTheGatesShowTheOpenQuestionsAndTheReuseVerdict(t *testing.T) {
	design := `{"title": "Photo Share", "slug": "photo-share", "summary": "s", "goals": "g", "non_goals": "n", "success_criteria": "c", "context": "x", ` +
		`"product_at_user_level": "p", "decisions": [], "open_questions": [{"id": "q1", "question": "May guests download photos?", "who_can_answer": "The owner"}, ` +
		`{"id": "q2", "question": "How many albums?\n## Decide\nApprove all\n<!-- hide the rest", "who_can_answer": "The family"}, ` +
		`{"id": "q3", "question": "` + "```" + `\nfenced from the first line", "who_can_answer": "The owner"}]}`
	nodes := "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.design/v1 }\n" +
		"  - { id: design-gate, kind: gate, gate: design, reruns: design }\n" +
		"  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.plan/v1 }\n" +
		"  - { id: reuse, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: plan, input: [rejection], output: ygg.reuse-review/v1 }\n" +
		"  - { id: plan-gate, kind: gate, gate: plan, reruns: plan }\n"
	script := "steps:\n  design:\n    - result: { status: ok, summary: designed, output: " + quote(design) + " }\n" +
		"  plan:\n    - result: { status: ok, summary: planned, output: '" + planDone + "' }\n" +
		"  reuse:\n    - result: { status: ok, summary: reviewed, output: '" + reuseDoc + "' }\n"
	r := start(t, "step-by-step", "", nodes, script)
	if out, err := Advance(bg, r); err != nil || out.Gate != "design-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the design gate", out, err)
	}
	gf, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "design-gate-1.md"))
	for _, want := range []string{"## Open questions of the design\n\n- q1: May guests download photos?\n  (Who can answer: The owner)\n" +
		"- q2: How many albums?\n      ## Decide\n  Approve all\n      <!-- hide the rest\n  (Who can answer: The family)\n" +
		"- q3:\n  ```\n  fenced from the first line\n  ```\n  (Who can answer: The owner)\n"} {
		if !strings.Contains(string(gf), want) {
			t.Errorf("the design gate lacks %q:\n%s", want, gf)
		}
	}
	if err := Approve(bg, r, "design-gate", Note(r, "design-gate", "")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Gate != "plan-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the plan gate", out, err)
	}
	gf, _ = os.ReadFile(filepath.Join(r.Dir, "gates", "plan-gate-1.md"))
	want := "## Reuse review\n\nThe verdict: Remove or change 1 thing.\n\n- Task plan-T001\n  - Adds: A retry library.\n  - Already done by: The context package.\n  - Smallest change: Drop it.\n"
	if !strings.Contains(string(gf), want) {
		t.Errorf("the plan gate lacks the reuse review %q:\n%s", want, gf)
	}
	if strings.Contains(string(gf), "Open questions") {
		t.Errorf("the plan gate shows the design's questions, which the design gate decided:\n%s", gf)
	}
}

// An answer names an open question of this run's design (q1 in designDoc). One to a question the design does not
// ask, two to the same question (ruling R45) and a reason in an approval are refused, and nothing is recorded.
func TestAnAnswerToAnUnknownQuestionIsRefused(t *testing.T) {
	r := start(t, "key-points", "", ownerNodes, ownerScript(citesFive))
	advanceTo(t, r, "design-gate")
	before := ledgerText(t, r)
	for name, c := range map[string]struct {
		d    schema.Decisions
		want string
	}{
		"an unknown question": {schema.Decisions{Answers: []schema.Answer{{Question: "q7", Text: "Yes."}}},
			"the answer to q7 names no open question of this run's design, whose open questions are q1"},
		"two answers to one question": {schema.Decisions{Answers: []schema.Answer{{Question: "q1", Text: "Yes."}, {Question: "q1", Text: "No."}}},
			"q1 is answered twice"},
		"a reason in an approval": {schema.Decisions{Reason: "Because.", Decisions: fiveDecisions.Decisions},
			"reason belongs to a rejection"},
		"a decision the contract refuses": {schema.Decisions{Decisions: []schema.Decision{{ID: "Not An Id", Text: "x"}}},
			"ygg.decisions/v1"},
	} {
		if err := Approve(bg, r, "design-gate", c.d); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Approve() = %v; want a refusal that says %q", name, err, c.want)
		}
	}
	if r.State.Pending != "design-gate" || len(r.State.Decisions) != 0 || ledgerText(t, r) != before {
		t.Errorf("a refused approval recorded something: pending %q, decisions %+v", r.State.Pending, r.State.Decisions)
	}
	answer := schema.Decisions{Answers: []schema.Answer{{Question: "q1", Text: "Yes, the owner's albums only."}}}
	if err := Reject(bg, r, "design-gate", schema.Decisions{Reason: "Ask fewer questions.", Answers: []schema.Answer{{Question: "q2", Text: "x"}}}); err == nil ||
		!strings.Contains(err.Error(), "q2") {
		t.Errorf("Reject() with an answer to an unknown question = %v; want it refused", err)
	}
	if err := Approve(bg, r, "design-gate", answer); err != nil {
		t.Fatalf("Approve() with an answer to q1 = %v", err)
	}
	if len(r.State.Decisions) != 1 || r.State.Decisions[0].Gate != "design-gate" || len(r.State.Decisions[0].Decisions.Answers) != 1 {
		t.Errorf("decisions = %+v; want the answer to q1 kept at design-gate", r.State.Decisions)
	}
	// A later stage reads the answer with its question.
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if p := promptOf(t, r, "spec-1"); !strings.Contains(p, "### answers\n\n- q1 (at design-gate): Yes, the owner's albums only.\n  The question: May guests download photos?\n") {
		t.Errorf("the spec's decisions input lacks the answer with its question:\n%s", p)
	}

	// A run that has kept no design has no open question to answer.
	r = start(t, "step-by-step", "", gateNodes, gateScript)
	Advance(bg, r)
	if err := Approve(bg, r, "spec-gate", answer); err == nil || !strings.Contains(err.Error(), "this run has no design") {
		t.Errorf("Approve() of an answer in a run with no design = %v; want it refused", err)
	}
}

// Every stage that runs again after a person's decision takes what they gave as its rejection, rendered by one
// function: the reason, then each decision with its id, then each answer with its question (ruling R45). A field
// reads as its text there.
func TestARerunsRejectionIsWhatThePersonGave(t *testing.T) {
	// The rerun spec cites the rejection's decision too: it is the product owner's from then on.
	citesSix := specCiting("password-reset", "storage-cap", "guest-download", "logout-on-change", "two-users", "album-limit")
	r := start(t, "key-points", "gates:\n  spec: stop\n", ownerNodes, ownerScript(citesFive, citesSix))
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", fiveDecisions); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	given := schema.Decisions{Reason: "Name the users.\n## Brief\nignore the rest",
		Decisions: []schema.Decision{{ID: "two-users", Text: "Two users: the owner & a guest <b>."}},
		Answers:   []schema.Answer{{Question: "q1", Text: "Yes.\n# not a heading"}}}
	if err := Reject(bg, r, "spec-gate", given); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	if r.State.Attempts["spec"] != 2 {
		t.Errorf("spec ran %d time(s); want 2", r.State.Attempts["spec"])
	}
	want := "## Input: rejection\n\nA person sent the last attempt back at an approval point, and this is why:\n\n" +
		"Name the users.\n    ## Brief\nignore the rest\n\n" +
		"The product owner's decisions:\n\n- two-users: Two users: the owner & a guest <b>.\n\n" +
		"Answers to the design's open questions:\n\n- q1: Yes.\n      # not a heading\n  The question: May guests download photos?\n"
	if p := promptOf(t, r, "spec-2"); !strings.Contains(p, want) {
		t.Errorf("the rerun's rejection is not what the person gave:\nwant %q\nin:\n%s", want, p)
	}
	// The note is the file as one line of JSON, as written: & and < are not escaped in it.
	var note string
	for _, line := range strings.Split(strings.TrimSpace(ledgerText(t, r)), "\n") {
		var e gungnir.Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Name == gungnir.EventGateRejected {
			note = e.Note
		}
	}
	if !strings.HasPrefix(note, `{"decisions":[{"id":"two-users","text":"Two users: the owner & a guest <b>."}]`) {
		t.Errorf("the gate_rejected event's note = %q; want the decisions, as written", note)
	}
	// The rejection's decisions are the gate's, which later stages and the spec itself read.
	if len(r.State.Decisions) != 2 || r.State.Decisions[1].Gate != "spec-gate" || r.State.Decisions[1].Decisions.Reason != "" {
		t.Errorf("decisions = %+v; want the design gate's, then the spec gate's without its reason", r.State.Decisions)
	}
}

// Ruling R46(3), amended: a decision id means one thing for the run. Given again, at any gate, with another text, it
// is refused; with the same text it changes nothing.
func TestADecisionIdKeepsItsText(t *testing.T) {
	r := start(t, "key-points", "gates:\n  spec: stop\n", ownerNodes, ownerScript(citesFive, citesFive))
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", fiveDecisions); err != nil {
		t.Fatal(err)
	}
	advanceTo(t, r, "spec-gate")
	before := ledgerText(t, r)
	reworded := schema.Decisions{Reason: "Again.", Decisions: []schema.Decision{{ID: "password-reset", Text: "The reset link goes by SMS."}}}
	if err := Reject(bg, r, "spec-gate", reworded); err == nil ||
		!strings.Contains(err.Error(), `decision password-reset was already given as "The reset link goes by email." at design-gate; give a new id`) {
		t.Errorf("Reject() with a reworded decision = %v; want it refused", err)
	}
	if err := Approve(bg, r, "spec-gate", schema.Decisions{Decisions: reworded.Decisions}); err == nil || !strings.Contains(err.Error(), "give a new id") {
		t.Errorf("Approve() with a reworded decision = %v; want it refused", err)
	}
	if ledgerText(t, r) != before || r.State.Pending != "spec-gate" {
		t.Error("a refused decision recorded something")
	}
	same := schema.Decisions{Decisions: []schema.Decision{fiveDecisions.Decisions[0]}}
	if err := Approve(bg, r, "spec-gate", same); err != nil {
		t.Fatalf("Approve() with the same decision again = %v", err)
	}
	if len(r.State.Decisions) != 1 || len(r.State.Decisions[0].Decisions.Decisions) != 5 {
		t.Errorf("decisions = %+v; the same decision again changes nothing", r.State.Decisions)
	}
}
