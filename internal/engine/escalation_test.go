package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// passesAt is an implementer whose work fails the unit check in every attempt but the done-th, which
// writes what the check accepts; 0 means it never does.
func passesAt(done int) string {
	var b strings.Builder
	b.WriteString("steps:\n  build:\n")
	for i := 1; i <= 6; i++ {
		word := "todo"
		if i == done {
			word = "done"
		}
		b.WriteString("    - actions: [ " + write("build.txt", word) + " ]\n      result: { status: ok, usage: { in: 10, out: 1, turns: 1 }, output: '{\"summary\": \"done\"}' }\n")
	}
	return b.String()
}

func neverPasses() string { return passesAt(0) }

func TestALoopThatRunsOutOfAttemptsStopsTheRunForAPersonInEveryMode(t *testing.T) {
	for _, mode := range []string{"step-by-step", "key-points", "unattended"} {
		r := startLoop(t, mode, neverPasses())
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "build" || r.State.PendingGate != gungnir.GateEscalation || r.State.Position != 0 {
			t.Fatalf("%s: Advance() = %+v, %v, gate %s; want a stop at escalation", mode, out, err, r.State.PendingGate)
		}
		rec := r.State.Loops["build"]
		if rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonExhausted || r.State.Attempts["build"] != 3 {
			t.Errorf("%s: loop record %+v, %d sessions; want 3 attempts and attempts-exhausted", mode, rec, r.State.Attempts["build"])
		}
		text := ledgerText(t, r)
		want := `"ygg.result":"stopped"`
		if mode == "unattended" {
			want = `"ygg.result":"blocked"`
		}
		if !strings.Contains(text, `"event":"escalation"`) || !strings.Contains(text, want) || !strings.Contains(text, "attempts-exhausted") || !strings.Contains(text, `"event":"gate_pending"`) {
			t.Errorf("%s: the ledger lacks an escalation that is %s:\n%s", mode, want, text)
		}
		gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-3.md"))
		for _, want := range []string{"Escalation at loop build", "3 attempts ended with 1 blocking finding(s) open", "- check-unit-result (blocker, from unit)",
			"ygg reject " + r.State.ID + ` build --reason "..." | --decisions <file>`, "ygg approve " + r.State.ID + ` build [--note "..." | --decisions <file>]`} {
			if !strings.Contains(string(gate), want) {
				t.Errorf("%s: the gate file lacks %q:\n%s", mode, want, gate)
			}
		}
		if !Rejectable(r) || !Approvable(r) {
			t.Errorf("%s: an exhausted loop takes both a rejection and an approval", mode)
		}
	}
}

func TestRejectingAnEscalatedLoopGivesItAFullSetOfAttemptsWithTheOperatorsGuidance(t *testing.T) {
	// Attempts 1 to 3 fail; so do 4 and 5, which the rejection allows; the sixth, the last it allows, passes.
	r := startLoop(t, "key-points", passesAt(6))
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v", out)
	}
	if err := Reject(context.Background(), r, "build", Reason("  ")); err == nil {
		t.Error("a rejection with no reason was accepted")
	}
	if err := Reject(context.Background(), r, "build", Reason("The check wants the word done, in lower case.")); err != nil {
		t.Fatal(err)
	}
	if rec := r.State.Loops["build"]; rec.Escalation != nil || rec.State.Limit != 6 || rec.State.Attempt != 4 {
		t.Errorf("after the rejection: %+v; want attempts 4 to 6 and no escalation", rec)
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done after the rejection", out, err)
	}
	if got := promptOf(t, r, "build-4"); !strings.Contains(got, "## Input: rejection\n\nA person sent the last attempt back") || !strings.Contains(got, "in lower case") || !strings.Contains(got, `"id": "check-unit-result"`) {
		t.Errorf("the fourth attempt was told:\n%s", got)
	}
	text := ledgerText(t, r)
	if !strings.Contains(text, `"event":"gate_rejected"`) || !strings.Contains(text, `"event":"finding_closed"`) || !strings.Contains(text, `"ygg.finding.attempts":`) {
		t.Errorf("the ledger lacks the rejection or the close:\n%s", text)
	}
}

func TestApprovingAnExhaustedLoopGoesOnWithTheFindingsOpen(t *testing.T) {
	nodes := loopNode + "  - { id: after, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	script := neverPasses() + "  after:\n    - result: { status: ok, summary: went on, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: nodes, script: script})
	Advance(context.Background(), r)
	if err := Approve(context.Background(), r, "build", Note(r, "build", "the check is too strict; ship it")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := r.State.Summaries["build"]; !strings.Contains(got, "accepted by the operator") || !strings.Contains(got, "1 finding(s) open") {
		t.Errorf("summary = %q", got)
	}
	if f := r.State.Loops["build"].State.Book.Records["check-unit-result"]; f == nil || f.Status != gungnir.FindingOpen {
		t.Errorf("record = %+v; an approval closes nothing", f)
	}
	if text := ledgerText(t, r); strings.Contains(text, `"event":"finding_closed"`) || !strings.Contains(text, "the check is too strict") {
		t.Errorf("the ledger:\n%s", text)
	}
}

// The exit criterion: a malformed reviewer output stops the loop, and is never a pass. Each time the reviewer is
// asked, an answer that is not valid is asked for once more with its errors; asked twice, it escalates.
func TestAMalformedReviewerOutputStopsTheLoopAndIsNeverAPass(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: \"LGTM, ship it\" }\n    - result: { status: ok, output: \"Looks fine to me.\" }\n" +
		"    - result: { status: ok, output: \"Still fine.\" }\n    - result: { status: ok, output: \"Fine.\" }\n" +
		"    - result: { status: ok, report: [] }\n"
	r := startReview(t, "unattended", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateEscalation {
		t.Fatalf("Advance() = %+v, %v; want a stop at escalation", out, err)
	}
	rec := r.State.Loops["build"]
	if rec.State.Passed || rec.Escalation.Reason != jormungandr.ReasonUnreadable || r.State.Attempts["build-spec"] != 4 || r.State.Attempts["build"] != 1 {
		t.Errorf("record %+v; reviewer sessions %d, implementer sessions %d; want the reviewer asked twice, each with a repair, and no second implementer attempt",
			rec, r.State.Attempts["build-spec"], r.State.Attempts["build"])
	}
	if !strings.Contains(rec.Escalation.Detail, "reviewer spec") {
		t.Errorf("detail = %q", rec.Escalation.Detail)
	}
	if r.State.Status == run.StatusDone || strings.Contains(r.State.Summaries["build"], "passed") {
		t.Error("an answer nobody could read was taken for a pass")
	}
	// Nobody can approve on the reviewer's behalf, and the refusal changes nothing.
	before := ledgerText(t, r)
	if err := Approve(context.Background(), r, "build", Note(r, "build", "just go")); err == nil || !strings.Contains(err.Error(), "can't be read") {
		t.Errorf("Approve() = %v, want a refusal", err)
	}
	if Approvable(r) || !Rejectable(r) || ledgerText(t, r) != before {
		t.Error("the refusal changed the run, or approve was offered")
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	if !strings.Contains(string(gate), "Approving is not offered") || strings.Contains(string(gate), "ygg approve") {
		t.Errorf("the gate file offers an approval:\n%s", gate)
	}
	// Sending it back asks the reviewer again, and the loop goes on from there.
	if err := Reject(context.Background(), r, "build", Reason("I fixed the reviewer's prompt.")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone || r.State.Attempts["build"] != 1 {
		t.Errorf("Advance() = %+v, %v after the rejection; the implementer must not run again (%d sessions)", out, err, r.State.Attempts["build"])
	}
}

func TestAReviewerReportThatDropsAFindingOrClosesItWithoutProofStopsTheLoopToo(t *testing.T) {
	for name, second := range map[string]string{
		"dropped":         "[]",
		"closed no proof": `[ { id: spec-1, source: spec, severity: major, location: "build.txt:1", claim: c, status: fixed, proof: "it is fine now" } ]`,
		"closed no check": `[ ` + fixedFinding("build.txt:1 is right") + ` ]`,
	} {
		script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - actions: [ " + write("build.txt", "done again") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
			"  build-spec:\n    - result: { status: ok, report: [ " + majorFinding + " ] }\n" +
			"    - result: { status: ok, report: " + second + " }\n    - result: { status: ok, report: " + second + " }\n"
		r := startReview(t, "key-points", script)
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateEscalation || r.State.Loops["build"].Escalation.Reason != jormungandr.ReasonUnreadable {
			t.Errorf("%s: Advance() = %+v, %v; want a stop: the finding can't vanish or close on the reviewer's word alone", name, out, err)
		}
		if f := r.State.Loops["build"].State.Book.Records["spec-1"]; f == nil || f.Status != gungnir.FindingOpen {
			t.Errorf("%s: record = %+v; the finding must stay open", name, f)
		}
	}
}

func TestASendBackAfterAPassedLoopStartsTheLoopOver(t *testing.T) {
	nodes := loopNode + "  - { id: feature-gate, kind: gate, gate: feature }\n"
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done, with the footer") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{mode: "step-by-step", pack: unitCheck, nodes: nodes, script: script})
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped || out.Gate != "feature-gate" {
		t.Fatalf("Advance() = %+v", out)
	}
	if err := Reject(context.Background(), r, "feature-gate", Reason("Add a footer.")); err != nil {
		t.Fatal(err)
	}
	if _, kept := r.State.Loops["build"]; kept || r.State.Position != 0 {
		t.Errorf("the loop record survived a rejection: %+v at position %d", r.State.Loops, r.State.Position)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || out.Gate != "feature-gate" {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := promptOf(t, r, "build-2"); !strings.Contains(got, "Add a footer.") || !strings.Contains(got, "## Input: rejection\n\nA person sent the last attempt back") {
		t.Errorf("the loop's new first attempt was told:\n%s", got)
	}
	if r.State.Loops["build"].State.Attempt != 1 {
		t.Errorf("the new loop is at attempt %d, want 1", r.State.Loops["build"].State.Attempt)
	}
}

func TestAnEscalatedLoopSurvivesAReopenedRun(t *testing.T) {
	r := startLoop(t, "key-points", neverPasses())
	Advance(context.Background(), r)
	r = reopen(t, r)
	if r.State.PendingGate != gungnir.GateEscalation || r.State.Loops["build"].Escalation == nil || !Rejectable(r) || !Approvable(r) {
		t.Fatalf("after reopening: gate %s, loop %+v", r.State.PendingGate, r.State.Loops["build"])
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped {
		t.Errorf("Advance() = %+v, %v; a resume of a stopped run reports its stop", out, err)
	}
	if err := Approve(context.Background(), r, "build", Note(r, "build", "")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}

const blockedReview = `'{"blocked": {"reason": "I cannot read the diff."}}'`

// A reviewer that answers twice in a row that it could not review stops the loop with its own reason (ruling R19,
// amended), and a person may accept the work without its report.
func TestAReviewerThatIsBlockedTwiceStopsTheLoopForAPersonWhoMayApprove(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: " + blockedReview + " }\n    - result: { status: ok, output: " + blockedReview + " }\n"
	r := startReview(t, "key-points", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateEscalation {
		t.Fatalf("Advance() = %+v, %v; want a stop at escalation", out, err)
	}
	rec := r.State.Loops["build"]
	if rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonReviewerBlocked || !strings.Contains(rec.Escalation.Detail, "reviewer spec is blocked: I cannot read the diff.") {
		t.Fatalf("escalation = %+v; want reviewer-blocked, naming the reviewer and its reason", rec.Escalation)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	for _, want := range []string{"Reason: reviewer-blocked", "I cannot read the diff.", "The reviewer answered that it could not review the work",
		"ygg approve " + r.State.ID + ` build [--note "..." | --decisions <file>]`} {
		if !strings.Contains(string(gate), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gate)
		}
	}
	if strings.Contains(string(gate), "can't be read") {
		t.Errorf("the gate file calls a blocked reviewer unreadable:\n%s", gate)
	}
	if !Approvable(r) || !Rejectable(r) {
		t.Fatal("a blocked reviewer takes a person's ruling")
	}
	if err := Approve(context.Background(), r, "build", Note(r, "build", "I read the diff myself")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if got := r.State.Summaries["build"]; !strings.Contains(got, "accepted by the operator after 1 attempt(s)") {
		t.Errorf("summary = %q", got)
	}
}

// Rejecting a blocked reviewer asks it again on the same work: the implementer does not run again.
func TestRejectingABlockedReviewerAsksItAgainOnTheSameWork(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: " + blockedReview + " }\n    - result: { status: ok, output: " + blockedReview + " }\n" +
		"    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v; want a stop", out)
	}
	if err := Reject(context.Background(), r, "build", Reason("The diff is in the leaf now.")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone || r.State.Attempts["build"] != 1 || r.State.Attempts["build-spec"] != 3 {
		t.Errorf("Advance() = %+v, %v; implementer sessions %d, reviewer sessions %d; want done with 1 and 3", out, err, r.State.Attempts["build"], r.State.Attempts["build-spec"])
	}
}

// Ruling R52: a person's rejection of a blocked reviewer answers that reviewer: its re-run's rejection input holds the
// reason, and the implementer's next attempt does not. The gate file says so.
func TestARejectionOfABlockedReviewerGoesToThatReviewer(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done v2") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: " + blockedReview + " }\n    - result: { status: ok, output: " + blockedReview + " }\n" +
		"    - result: { status: ok, report: [ " + majorFinding + " ] }\n" +
		"    - result: { status: ok, report: [ " + fixedFinding("build.txt:1 now says what the brief asks; check unit passed") + " ] }\n"
	r := startReview(t, "key-points", script)
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v; want a stop", out)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	if want := "The reason goes to the reviewer spec, as its rejection input"; !strings.Contains(string(gate), want) {
		t.Errorf("the gate file lacks %q:\n%s", want, gate)
	}
	const reason = "The diff is in the leaf now."
	if err := Reject(context.Background(), r, "build", Reason(reason)); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if p := promptOf(t, r, "build-spec-3"); !strings.Contains(p, "## Input: rejection\n\nA person sent the last attempt back at an approval point, and this is why:\n\n"+reason) {
		t.Errorf("the reviewer's re-run lacks the person's reason:\n%s", p)
	}
	if p := promptOf(t, r, "build-2"); strings.Contains(p, reason) {
		t.Errorf("the implementer's next attempt holds the reason meant for the reviewer:\n%s", p)
	}
	if len(r.State.Reasons) != 0 {
		t.Errorf("reasons after the loop passed = %v; want none", r.State.Reasons)
	}
}

// A finding's fields are a reviewer's words: in the gate file its claim is its text, and cannot start a heading or an
// HTML block that would hide the Decide section.
func TestAFindingCannotForgeASectionOfTheEscalationFile(t *testing.T) {
	hostile := `{ id: "spec-1", source: spec, severity: major, location: "build.txt:1", claim: "wrong\n## x\n<!-- open", status: open }`
	script := "steps:\n  build:\n"
	for i := 0; i < 3; i++ {
		script += "    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	}
	script += "  build-spec:\n"
	for i := 0; i < 3; i++ {
		script += "    - result: { status: ok, report: [ " + hostile + " ] }\n"
	}
	r := startReview(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || r.State.Loops["build"].Escalation.Reason != jormungandr.ReasonExhausted {
		t.Fatalf("Advance() = %+v, %v; want a stop for attempts run out", out, err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "gates", "build-3.md"))
	if err != nil {
		t.Fatal(err)
	}
	headings, html := parsedMarkdown(b)
	want := []string{"# Escalation at loop build (escalation)", "## What happened", "## Findings still open", "## Cost so far", "## Decide"}
	if html != 0 || !slices.Equal(headings, want) {
		t.Errorf("goldmark reads %d HTML block(s) and the headings %q; want none, and %q:\n%s", html, headings, want, b)
	}
	if !strings.Contains(string(b), "- spec-1 (major, from spec) at build.txt:1\n  wrong\n") {
		t.Errorf("the gate file lacks the finding:\n%s", b)
	}
}

// Ruling R52 (amended): a person's rejection of an unreadable reviewer answers that reviewer too: its re-run's
// rejection input holds the reason, and the implementer's next attempt does not. The gate file says so.
func TestARejectionOfAnUnreadableReviewerGoesToThatReviewer(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done v2") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: \"LGTM, ship it\" }\n    - result: { status: ok, output: \"Looks fine to me.\" }\n" +
		"    - result: { status: ok, output: \"Still fine.\" }\n    - result: { status: ok, output: \"Fine.\" }\n" +
		"    - result: { status: ok, report: [ " + majorFinding + " ] }\n" +
		"    - result: { status: ok, report: [ " + fixedFinding("build.txt:1 now says what the brief asks; check unit passed") + " ] }\n"
	r := startReview(t, "key-points", script)
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped || r.State.Loops["build"].Escalation.Reason != jormungandr.ReasonUnreadable {
		t.Fatalf("Advance() = %+v; want a stop for an unreadable reviewer", out)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	if want := "The reason goes to the reviewer spec, as its rejection input"; !strings.Contains(string(gate), want) {
		t.Errorf("the gate file lacks %q:\n%s", want, gate)
	}
	const reason = "Answer with a JSON review."
	if err := Reject(context.Background(), r, "build", Reason(reason)); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if p := promptOf(t, r, "build-spec-5"); !strings.Contains(p, "## Input: rejection\n\nA person sent the last attempt back at an approval point, and this is why:\n\n"+reason) {
		t.Errorf("the reviewer's re-run lacks the person's reason:\n%s", p)
	}
	if p := promptOf(t, r, "build-2"); strings.Contains(p, reason) {
		t.Errorf("the implementer's next attempt holds the reason meant for the reviewer:\n%s", p)
	}
	if len(r.State.Reasons) != 0 {
		t.Errorf("reasons after the loop passed = %v; want none", r.State.Reasons)
	}
}

// Each escalation's Decide section says who gets a rejection's reason: the reviewer that could not answer, or the
// implementer's next attempt.
func TestEachDecideTextSaysWhoGetsTheReason(t *testing.T) {
	n := schema.Node{ID: "build", Attempts: 3}
	for reason, want := range map[string]string{
		jormungandr.ReasonExhausted:       "The reason goes to the implementer's next attempt",
		jormungandr.ReasonDisputed:        "The reason goes to the implementer's next attempt",
		jormungandr.ReasonBlocked:         "it goes to the implementer's next attempt",
		jormungandr.ReasonReviewerBlocked: "The reason goes to the reviewer spec, as its rejection input",
		jormungandr.ReasonUnreadable:      "The reason goes to the reviewer spec, as its rejection input",
	} {
		if got := decideText("r", n, reason, &run.Escalation{Reason: reason, Reviewer: "spec"}); !strings.Contains(got, want) {
			t.Errorf("%s: the Decide text lacks %q:\n%s", reason, want, got)
		}
	}
}
