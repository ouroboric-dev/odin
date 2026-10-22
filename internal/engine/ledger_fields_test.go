package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

// Spec §10.2 counts a reviewer's veto and an ignored dispute. The ledger names which raised a finding, and a
// dispute that could not be used has an event of its own, so ygg metrics reads fields and parses no text.
func TestTheLedgerSaysWhetherACheckOrAReviewerRaisedAFinding(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n" +
		"    - actions: [ " + write("build.txt", "done again") + " ]\n      result: { status: ok }\n" +
		"  build-spec:\n    - result: { status: ok, report: [ " + majorFinding + " ] }\n" +
		"    - result: { status: ok, report: [ " + fixedFinding("build.txt:1 now says what the brief asks; check unit passed") + " ] }\n"
	r := startReview(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	text := ledgerText(t, r)
	for _, want := range []string{
		`"event":"finding_raised"`, `"ygg.finding.source":"spec"`, `"ygg.finding.origin":"review"`,
		`"event":"finding_closed"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `"event":"finding_`) && !strings.Contains(line, `"ygg.finding.origin":"`) {
			t.Errorf("a finding line names no origin:\n%s", line)
		}
	}
}

func TestACheckFindingHasTheOriginCheck(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok }\n"
	r := startLoop(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"ygg.finding.id":"check-unit"`) || !strings.Contains(text, `"ygg.finding.origin":"check"`) {
		t.Errorf("the check finding names no origin:\n%s", text)
	}
}

func TestAnIgnoredDisputeIsADisputeIgnoredEventNotALoopAttemptWithText(t *testing.T) {
	r := startDispute(t, disputeScript(`'{"disputes": [{"id": "spec-1"}]}'`, 2))
	Advance(context.Background(), r) //nolint:errcheck // the run's end is not what this test reads
	text := ledgerText(t, r)
	if !strings.Contains(text, `"event":"dispute_ignored"`) || !strings.Contains(text, `"ygg.note":"dispute ignored: `) {
		t.Errorf("the ledger has no dispute_ignored line that says why:\n%s", text)
	}
	if strings.Contains(text, `"ygg.result":"dispute ignored`) {
		t.Errorf("the dispute is still written as a loop_attempt with text:\n%s", text)
	}
}

func TestTheLedgerKeepsTheBenchScoreAsNumbersAndAnUnmeasuredIndexCarriesNone(t *testing.T) {
	r := startPack(t, "key-points", "", benchPack, shipNodes, goModScript)
	writeBench(t, r, rightBench)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"routing_index_built"`, `"ygg.routing.bench":1`, `"ygg.routing.threshold":0.8`, `"ygg.routing.baseline":`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	plain := startPack(t, "key-points", "", packChecks, shipNodes, goModScript)
	if out, err := Advance(bg, plain); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if text := ledgerText(t, plain); !strings.Contains(text, `"event":"routing_index_built"`) || strings.Contains(text, `"ygg.routing.bench"`) {
		t.Errorf("an index shipped unmeasured must say no score:\n%s", text)
	}
}
