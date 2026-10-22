package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode"

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

// Plan decision 6: every piece of agent text is quoted or on one line, with no control character, and it opens
// no heading, ends no block or quote, and fetches nothing when report.md is rendered.
func TestEveryPieceOfAgentTextIsCleanedAndEscapedWhereverItIsPrinted(t *testing.T) {
	hostile := []string{"\x1b[31mred\x1b[0m", "bell\a", "c1\u009bx", "rtl‮evil", "zw​sp", "iso⁦x⁩", "x\r# h", "a\r\nb",
		"a # h", "a # h", "a\u0085# h", "```", "x`y", "![i](http://h/b)", "[l](javascript:x)", "<script>", "<img src=x onerror=alert(1)>",
		"<!--", "a\\|b", "ends\\", "# h", "- item", "1. item", "&amp;", "*b* _i_ ~s~"}
	m0, st0 := fixture()
	var base bytes.Buffer
	Write(&base, m0, st0)
	baseHeads := countPrefix(base.String(), "#")
	for _, h := range hostile {
		for field := 0; field < 14; field++ {
			m, st := fixture()
			f := st.Loops["g-t1-implement"].State.Book.Records["spec-1"]
			e := st.Loops["g-t2-implement"].Escalation
			switch field {
			case 0:
				f.Claim = h
			case 1:
				f.Proof = h
			case 2:
				f.OverruledBy = h
			case 3:
				f.ID = h
			case 4:
				f.Source = h
			case 5:
				m.BlockedTasks[0].Why = h
			case 6:
				e.Reason = h
			case 7:
				e.Detail = h
			case 8:
				f.Location = h
			case 9:
				m.Interventions.Items[0].Note = h
			case 10:
				m.Interventions.Items[0].Kind = h
			case 11:
				m.Interventions.Items[1].Node = h
			case 12:
				f.Severity = gungnir.Severity(h)
			case 13:
				m.BlockedTasks[0].Task = h
			}
			var b bytes.Buffer
			if err := Write(&b, m, st); err != nil {
				t.Fatal(err)
			}
			out := b.String()
			where := func(msg string) string { return fmt.Sprintf("field %d, value %q: %s\n%s", field, h, msg, out) }
			for _, r := range out {
				if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
					t.Fatal(where(fmt.Sprintf("control or format rune %U", r)))
				}
			}
			if n := countPrefix(out, "#"); n != baseHeads {
				t.Fatal(where(fmt.Sprintf("%d lines start with # (the report writes %d)", n, baseHeads)))
			}
			if n := countPrefix(out, "```"); n != 2 {
				t.Fatal(where(fmt.Sprintf("%d fence lines, want 2", n)))
			}
			fenced := false
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "```") {
					fenced = !fenced
					continue
				}
				if fenced {
					continue // the metrics block is a fence: nothing in it is syntax
				}
				if i := strings.Index(l, "the check "); i >= 0 {
					l = l[:i] // the check name sits in a code span, where nothing is syntax
				}
				for _, bad := range []string{"![", "](", "<"} {
					for i := strings.Index(l, bad); i >= 0; {
						if i == 0 || l[i-1] != '\\' {
							t.Fatal(where(fmt.Sprintf("unescaped %q in %q", bad, l)))
						}
						j := strings.Index(l[i+1:], bad)
						if j < 0 {
							break
						}
						i += 1 + j
					}
				}
				if strings.HasPrefix(l, "|") && unescapedPipes(l) != 4 {
					t.Fatal(where(fmt.Sprintf("table row %q has %d pipes, want 4", l, unescapedPipes(l))))
				}
			}
		}
	}
}

func countPrefix(out, p string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, p) {
			n++
		}
	}
	return n
}

// unescapedPipes counts the pipes a table parser takes as a cell edge: those with an even run of backslashes
// before them.
func unescapedPipes(l string) int {
	n := 0
	for i := 0; i < len(l); i++ {
		if l[i] != '|' {
			continue
		}
		k := 0
		for j := i - 1; j >= 0 && l[j] == '\\'; j-- {
			k++
		}
		if k%2 == 0 {
			n++
		}
	}
	return n
}

func TestCodeSpanFenceIsLongerThanAnyRunOfBackticksInTheText(t *testing.T) {
	for in, want := range map[string]string{"unit": "`unit`", "x`y": "``x`y``", "a``b": "```a``b```", "`x": "`` `x ``", "x`": "`` x` ``", "a\x1bb": "`a b`"} {
		if got := code(in); got != want {
			t.Errorf("code(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteEndsEveryKindOfLineBreakAndKeepsEveryLineInTheQuote(t *testing.T) {
	got := quote("a\r# h\r\nb c d\u0085e\n\nf")
	if want := "> a\n> \\# h\n> b\n> c\n> d\n> e\n>\n> f"; got != want {
		t.Errorf("quote = %q, want %q", got, want)
	}
}
