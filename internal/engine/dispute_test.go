package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// printingCheck is a unit check that prints the test it ran when it passes, as the realms' test checks do.
const printingCheck = "checks:\n  unit: [sh, -c, \"grep -q done build.txt && echo 'ok TestFindIndex'\"]\n"

// findIndexClaim is the finding of run 20261020-190818-5b02ff: a reviewer says a method does not exist.
const findIndexClaim = `{ id: spec-1, source: spec, severity: blocker, location: "build.txt:1", claim: "findIndex is not a JavaScript method", status: open }`

func disputeOf(check, test string) string {
	return `'{"summary": "disputed spec-1", "disputes": [{"id": "spec-1", "check": "` + check + `", "test": "` + test + `", "evidence": "curl showed findIndex is defined; TestFindIndex runs it"}]}'`
}

// disputeScript is a loop whose reviewer raises findIndexClaim in every session it gets, and whose
// implementer disputes it with disputes in every attempt but the first.
func disputeScript(disputes string, attempts int) string {
	var b strings.Builder
	b.WriteString("steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n")
	for i := 2; i <= attempts; i++ {
		b.WriteString("    - result: { status: ok, output: " + disputes + " }\n")
	}
	b.WriteString("  build-spec:\n")
	for i := 1; i <= attempts; i++ {
		b.WriteString("    - result: { status: ok, report: [ " + findIndexClaim + " ] }\n")
	}
	return b.String()
}

func startDispute(t *testing.T, script string) *run.Run {
	t.Helper()
	return startWith(t, setup{mode: "key-points", pack: printingCheck, nodes: reviewLoop, script: script})
}

func TestADisputeSettledByAPassingCheckThatPrintsTheTestOverrulesTheFinding(t *testing.T) {
	t.Skip("typed-T20: a dispute holds only on a test the check's typed result lists as passed (jormungandr R23), and Check fills PassedTests from gleipnir's typed checks in T20")
	r := startDispute(t, disputeScript(disputeOf("unit", "TestFindIndex"), 3))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	rec := r.State.Loops["build"]
	if !rec.State.Passed || rec.State.Attempt != 2 {
		t.Errorf("loop = passed %v, attempt %d; want passed in attempt 2", rec.State.Passed, rec.State.Attempt)
	}
	f := rec.State.Book.Records["spec-1"]
	if f == nil || f.Status != gungnir.FindingOverruled {
		t.Fatalf("spec-1 = %+v; want overruled", f)
	}
	for _, want := range []string{"curl showed findIndex is defined", "check unit ran in attempt 2 and passed", "logs/build-2-unit.log"} {
		if !strings.Contains(f.Proof, want) {
			t.Errorf("the proof lacks %q: %s", want, f.Proof)
		}
	}
	if !strings.Contains(transcript(t, r, "build-2"), "Disputing a finding") || !strings.Contains(transcript(t, r, "build-2"), "The checks of this loop: unit") {
		t.Error("the second attempt was not told how to dispute, with the loop's checks")
	}
	if strings.Contains(transcript(t, r, "build-1"), "Disputing a finding") {
		t.Error("the first attempt has no finding to dispute and must not be told how")
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"finding_overruled"`, `"ygg.finding.id":"spec-1"`, `"ygg.finding.attempts":2`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	if !strings.Contains(r.State.Summaries["build"], "overruled") {
		t.Errorf("the summary %q does not mention the overruled finding", r.State.Summaries["build"])
	}
}

func TestADisputeWhoseTestIsNotInTheCheckOutputDoesNotHoldAndTheLoopEscalatesAsDisputed(t *testing.T) {
	r := startDispute(t, disputeScript(disputeOf("unit", "TestNotRun"), 3))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateEscalation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at escalation", out, err, r.State.PendingGate)
	}
	rec := r.State.Loops["build"]
	if rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonDisputed {
		t.Fatalf("escalation = %+v; want finding-disputed", rec.Escalation)
	}
	if got := rec.State.Book.Records["spec-1"].Status; got != gungnir.FindingOpen {
		t.Errorf("status = %s; the finding stays open", got)
	}
	if !strings.Contains(transcript(t, r, "build-3"), "Your disputes that did not hold") || !strings.Contains(transcript(t, r, "build-3"), "TestNotRun") {
		t.Error("the third attempt was not told the dispute did not hold, and why")
	}
	for _, want := range []string{"reviewer's claim: findIndex is not a JavaScript method", "implementer's evidence", "curl showed findIndex is defined", "can't show that TestNotRun"} {
		if !strings.Contains(rec.Escalation.Detail, want) {
			t.Errorf("the escalation detail lacks %q:\n%s", want, rec.Escalation.Detail)
		}
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-3.md"))
	for _, want := range []string{"Reason: finding-disputed", "curl showed findIndex is defined", "ygg approve " + r.State.ID + " build", "ygg reject " + r.State.ID + " build"} {
		if !strings.Contains(string(gate), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gate)
		}
	}
	if !Approvable(r) || !Rejectable(r) {
		t.Error("a disputed finding takes the operator's ruling: approve (overrule) or reject (send back)")
	}
}

func TestADisputeThatNamesACheckTheLoopDoesNotRunStopsTheRunAtOnce(t *testing.T) {
	r := startDispute(t, disputeScript(disputeOf("integration", "TestFindIndex"), 3))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v; want a stop", out, err)
	}
	rec := r.State.Loops["build"]
	if rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonDisputed || r.State.Attempts["build"] != 2 {
		t.Errorf("escalation %+v after %d sessions; want finding-disputed in attempt 2", rec.Escalation, r.State.Attempts["build"])
	}
	if !strings.Contains(ledgerText(t, r), "finding-disputed") {
		t.Error("the ledger lacks the escalation reason finding-disputed")
	}
	// The operator overrules: approve lets the work go on, and the finding stays on record.
	if err := Approve(context.Background(), r, "build", "findIndex exists"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("after the approval: %+v, %v", out, err)
	}
}

func TestADisputeOfACheckThatFailedDoesNotHoldAndSaysSo(t *testing.T) {
	// Attempt 2 breaks the work, so unit fails in it, and the implementer disputes all the same.
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n" +
		"    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: " + disputeOf("unit", "TestFindIndex") + " }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: " + disputeOf("unit", "TestFindIndex") + " }\n" +
		"  build-spec:\n    - result: { status: ok, report: [ " + findIndexClaim + " ] }\n    - result: { status: ok, report: [ " + findIndexClaim + " ] }\n"
	r := startWith(t, setup{mode: "key-points", pack: printingCheck, nodes: reviewLoop, script: script})
	Advance(context.Background(), r) //nolint:errcheck // the run's end is not what this test reads
	if got := transcript(t, r, "build-3"); !strings.Contains(got, "Your disputes that did not hold") || !strings.Contains(got, "check unit failed in attempt 2 and names no failing test") {
		t.Errorf("the third attempt was not told the check failed:\n%s", got)
	}
	// Until the check's typed result lists the tests that passed (typed-T20), no dispute holds, and the one in
	// attempt 3 leaves spec-1 open too.
	if got := r.State.Loops["build"].State.Book.Records["spec-1"]; got.Status != gungnir.FindingOpen {
		t.Errorf("spec-1 = %+v; no dispute settles it", got)
	}
}

// notOpenDispute disputes a finding no one raised. A malformed dispute is no longer ignored: the answer is not a
// ygg.implement/v1 document, and that is an agent failure (typed-T15).
const notOpenDispute = `'{"summary": "disputed", "disputes": [{"id": "spec-9", "check": "unit", "test": "TestFindIndex", "evidence": "it runs"}]}'`

func TestADisputeOfAFindingThatIsNotOpenIsIgnoredWithALedgerLine(t *testing.T) {
	r := startDispute(t, disputeScript(notOpenDispute, 2))
	Advance(context.Background(), r) //nolint:errcheck // the run's end is not what this test reads
	if text := ledgerText(t, r); !strings.Contains(text, "dispute ignored") {
		t.Errorf("the ledger lacks a line for the ignored dispute:\n%s", text)
	}
}
