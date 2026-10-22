package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

func fixture() (*metrics.Run, *run.State) {
	m := &metrics.Run{ID: "20261023-090000-a1b2c3", Status: "done", Mode: "key-points", Graph: "software", GraphVersion: "0.1.0",
		Factory: []string{"odin b8c9716fa818"}, Roles: map[string]metrics.Setting{}, Realms: map[string]string{},
		BlockedTasks: []metrics.BlockedTask{{Task: "g-T2", Why: "attempts-exhausted in implement"}},
		Interventions: metrics.Interventions{Escalations: 1, NotedApprovals: 1, Items: []metrics.Item{
			{Kind: "approval with a note", Node: "g-t1-contract-gate", Note: "the contract change is in the spec"},
			{Kind: "escalation", Node: "g-t2-implement", Note: "attempts-exhausted"}}}}
	st := &run.State{ID: m.ID, Ledger: ledger.Anchor{Seq: 26, Hash: "976c4a0eb053d2bf4182cb5c2e108a9d622ee44fc0eb4d164a6151a51a1bcf8b"},
		Loops: map[string]*run.LoopRecord{
			"g-t1-implement": {State: jormungandr.State{Book: jormungandr.Book{Records: map[string]*jormungandr.Record{
				"spec-1": {Finding: gungnir.Finding{ID: "spec-1", Source: "spec", Severity: gungnir.SeverityBlocker, Location: "build.txt:1",
					Claim: "findIndex is not a JavaScript method.\n# A heading the reviewer slipped in\n```\nescape", Status: gungnir.FindingOverruled,
					Proof: "curl showed findIndex is defined; TestFindIndex runs it"}, OverruledBy: "unit"}}}}},
			"g-t2-implement": {Escalation: &run.Escalation{Reason: "attempts-exhausted", Detail: "attempt 3 of 3 ended with 1 blocking finding open"}},
		}}
	return m, st
}

func render(t *testing.T) string {
	t.Helper()
	m, st := fixture()
	var b bytes.Buffer
	if err := Write(&b, m, st); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Spec §7.3: a blocked task is listed first in the run report.
func TestBlockedTasksComeBeforeEverythingElseAndANoneIsSaid(t *testing.T) {
	out := render(t)
	blocked, metricsAt := strings.Index(out, "## Blocked tasks"), strings.Index(out, "## Metrics")
	if blocked < 0 || metricsAt < 0 || blocked > metricsAt || !strings.Contains(out, "g-T2: attempts-exhausted in implement") {
		t.Errorf("the blocked tasks are not first:\n%s", out)
	}
	m, st := fixture()
	m.BlockedTasks = nil
	var b bytes.Buffer
	Write(&b, m, st)
	if !strings.Contains(b.String(), "## Blocked tasks\n\nNone.") {
		t.Errorf("a report with no blocked task must say so:\n%s", b.String())
	}
}

// B-115: an overruled finding is the implementer's word backed by one test. The reader sees both sides.
func TestAnOverruledFindingShowsTheReviewersClaimTheImplementersEvidenceAndTheCheck(t *testing.T) {
	out := render(t)
	for _, want := range []string{"## Findings a dispute overruled", "spec-1", "blocker", "from spec", "build.txt:1",
		"Reviewer's claim", "findIndex is not a JavaScript method", "Implementer's evidence", "TestFindIndex runs it", "Settled by the check `unit`"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
}

// An agent wrote the claim, and a blocked task's reason can hold agent text too. Whatever it holds must stay
// inside its quote or its block: it cannot open a heading or end a fence of the report.
func TestTextAnAgentWroteCannotBreakOutOfItsQuoteOrItsBlock(t *testing.T) {
	m, st := fixture()
	m.BlockedTasks[0].Why = "gave up\n```\n# injected heading"
	var b bytes.Buffer
	if err := Write(&b, m, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	fences := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "A heading the reviewer slipped in") && !strings.HasPrefix(l, "> ") {
			t.Errorf("an agent's line is outside its quote: %q", l)
		}
		if strings.HasPrefix(l, "```") {
			fences++
		}
		if strings.HasPrefix(l, "# injected") {
			t.Errorf("an injected heading is live: %q", l)
		}
	}
	if fences != 2 {
		t.Errorf("the report has %d fence lines, want 2 (the metrics block opens and closes once):\n%s", fences, out)
	}
}

func TestInterventionsAndGivenUpLoopsAreListedWithTheirReasons(t *testing.T) {
	out := render(t)
	for _, want := range []string{"## Interventions", "the contract change is in the spec", "g-t1-contract-gate",
		"## Loops that gave up", "g-t2-implement", "attempts-exhausted", "attempt 3 of 3 ended with 1 blocking finding open"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
}

func TestTheReportNamesTheLedgerPositionItWasReadFromAndIsTheSameEveryTime(t *testing.T) {
	out := render(t)
	if !strings.Contains(out, "ledger line 26") || !strings.Contains(out, "sha256 976c4a0eb053") {
		t.Errorf("the report does not say which ledger line its numbers come from:\n%s", out)
	}
	if again := render(t); again != out {
		t.Error("two renders of one run differ")
	}
}
