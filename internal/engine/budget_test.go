package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/budget"
	"github.com/ouroboric-dev/odin/internal/run"
)

var twoNodes = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"

const spendScript = `steps:
  a:
    - actions: [ { write: { path: a.txt, content: "a\n" } } ]
      result: { status: ok, summary: did a, usage: { in: 100, out: 20, cache: 1000, turns: 3, cost_usd: 0.25 } }
  b:
    - actions: [ { write: { path: b.txt, content: "b\n" } } ]
      result: { status: ok, summary: did b, usage: { in: 10, out: 1, turns: 1 } }
    - result: { status: ok, summary: did b again, usage: { in: 10, out: 1, turns: 1 } }
`

func caps(runTokens, runTurns, dayTokens, dayTurns int) string {
	return "budget:\n  per_run: { tokens: " + itoa(runTokens) + ", turns: " + itoa(runTurns) + " }\n  per_day: { tokens: " + itoa(dayTokens) + ", turns: " + itoa(dayTurns) + " }\n"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// reopen closes the run and opens it again from its files, as ygg resume or approve does, so a
// change to factory.yaml takes effect.
func reopen(t *testing.T, r *run.Run) *run.Run {
	t.Helper()
	id, project := r.State.ID, r.Project
	r.Close()
	r2, err := run.Open(context.Background(), project, id, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r2.Close() })
	return r2
}

func today(t *testing.T, r *run.Run) gungnir.Usage {
	t.Helper()
	u, err := r.Setup.Journal.Day(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestASessionsUsageIsCountedInTheRunAndInTheDaysJournal(t *testing.T) {
	r := start(t, "key-points", "", twoNodes, spendScript)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	want := gungnir.Usage{InTok: 110, OutTok: 21, CacheTok: 1000, Turns: 4, CostUSD: 0.25}
	if r.State.Usage != want {
		t.Errorf("run usage = %+v, want %+v", r.State.Usage, want)
	}
	if got := today(t, r); got != want {
		t.Errorf("the day's journal = %+v, want %+v", got, want)
	}
}

func TestAFailedSessionsUsageStillCounts(t *testing.T) {
	script := "steps:\n  a:\n    - error: timed out half way\n      usage: { in: 500, out: 50, turns: 4 }\n"
	r := start(t, "key-points", "", twoNodes, script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want failed", out, err)
	}
	if u := r.State.Usage; u.InTok != 500 || u.OutTok != 50 || u.Turns != 4 {
		t.Errorf("run usage = %+v; a session that timed out still spent its tokens", u)
	}
	if u := today(t, r); u.InTok != 500 || u.Turns != 4 {
		t.Errorf("the day's journal = %+v", u)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"node_failed"`) || !strings.Contains(ledgerText(t, r), `"gen_ai.usage.input_tokens":500`) {
		t.Errorf("the failed session's usage is not in the ledger:\n%s", ledgerText(t, r))
	}
}

func TestARunThatReachesItsCapStopsAtBudgetCapInEveryModeBeforeTheNextSession(t *testing.T) {
	for _, mode := range []string{"key-points", "unattended", "step-by-step"} {
		r := startWith(t, setup{mode: mode, nodes: twoNodes, script: spendScript, budget: caps(120, 100, 200000, 200)})
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap || out.Gate != "b" {
			t.Fatalf("%s: Advance() = %+v, %v, gate %s; want a stop at budget-cap before node b", mode, out, err, r.State.PendingGate)
		}
		if r.State.Attempts["b"] != 0 {
			t.Errorf("%s: node b started %d sessions over the cap", mode, r.State.Attempts["b"])
		}
		text := ledgerText(t, r)
		if !strings.Contains(text, `"event":"gate_pending"`) || !strings.Contains(text, "budget-cap") || !strings.Contains(text, "per-run tokens cap is reached: 120 used of 120") {
			t.Errorf("%s: the ledger doesn't say why:\n%s", mode, text)
		}
		gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "b-0.md"))
		if !strings.Contains(string(gate), "budget-cap") || !strings.Contains(string(gate), "factory.yaml") {
			t.Errorf("%s: the gate file doesn't tell the operator what to do:\n%s", mode, gate)
		}
	}
}

func TestApprovingBudgetCapDoesNotWaiveTheCapButRaisingItDoes(t *testing.T) {
	r := startWith(t, setup{nodes: twoNodes, script: spendScript, budget: caps(120, 100, 200000, 200)})
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v", out)
	}
	if err := Approve(context.Background(), r, "b", "go on"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap {
		t.Fatalf("Advance() = %+v, %v; approving does not lift a cap that is still reached", out, err)
	}
	if err := Reject(context.Background(), r, "b", "no"); err == nil {
		t.Error("a hard stop took a rejection")
	}
	// The operator raises the cap in factory.yaml; the run is opened again and approved.
	cfg, _ := os.ReadFile(r.Setup.ConfigPath)
	if err := os.WriteFile(r.Setup.ConfigPath, []byte(strings.Replace(string(cfg), "tokens: 120", "tokens: 1000", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	r = reopen(t, r)
	if err := Approve(context.Background(), r, "b", "raised the cap"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done after the cap was raised", out, err)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"config_changed"`) {
		t.Error("the raised cap is not in the ledger as a config change")
	}
}

func TestThePerDayCapCountsWhatOtherRunsSpent(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "spend")
	j := &budget.Journal{Dir: journal}
	if err := j.Append(budget.Entry{Run: "20261016-090000-aaaaaa", Project: "/another/project", Node: "x", Usage: gungnir.Usage{InTok: 1990, OutTok: 10, Turns: 5}}); err != nil {
		t.Fatal(err)
	}
	r := startWith(t, setup{nodes: twoNodes, script: spendScript, journal: journal, budget: caps(100000, 100, 2000, 200)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap || out.Gate != "a" || r.State.Attempts["a"] != 0 {
		t.Fatalf("Advance() = %+v, %v; want a stop at budget-cap before the first session", out, err)
	}
	if !strings.Contains(ledgerText(t, r), "per-day tokens cap is reached") {
		t.Errorf("the ledger doesn't name the day's cap:\n%s", ledgerText(t, r))
	}
}

func TestAJournalThatCannotBeReadStopsTheRun(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "spend")
	os.MkdirAll(journal, 0o700)
	os.WriteFile((&budget.Journal{Dir: journal}).File(time.Now()), []byte("not json\nnot json\n"), 0o600)
	r := startWith(t, setup{nodes: twoNodes, script: spendScript, journal: journal})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap || r.State.Attempts["a"] != 0 {
		t.Fatalf("Advance() = %+v, %v; a cap that can't be checked is reached", out, err)
	}
	if !strings.Contains(ledgerText(t, r), "can't be checked") {
		t.Errorf("the ledger doesn't say the journal is unreadable:\n%s", ledgerText(t, r))
	}
}

func TestASessionIsGivenOnlyTheTurnsTheCapsStillAllow(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, max_turns: 50 }\n  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app, max_turns: 2 }\n  - { id: c, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"
	script := "steps:\n  a:\n    - result: { status: ok, usage: { turns: 3 } }\n  b:\n    - result: { status: ok }\n  c:\n    - result: { status: ok }\n"
	r := startWith(t, setup{nodes: nodes, script: script, budget: caps(100000, 10, 200000, 200)})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	turns := map[string]int{}
	for _, n := range []string{"a", "b", "c"} {
		b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", n+"-1.json"))
		if err != nil {
			t.Fatal(err)
		}
		var tr struct {
			MaxTurns int `json:"max_turns"`
		}
		json.Unmarshal(b, &tr)
		turns[n] = tr.MaxTurns
	}
	// 10 turns in the run: a is bounded by its own 50 to the 10 left; b, after a spent 3, by its own 2; c by the 7 left.
	if turns["a"] != 10 || turns["b"] != 2 || turns["c"] != 7 {
		t.Errorf("max turns given = %v, want a:10 b:2 c:7", turns)
	}
}
