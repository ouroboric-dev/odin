package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/budget"
	"github.com/ouroboric-dev/odin/internal/run"
)

var twoNodes = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"

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
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, max_turns: 50, input: [rejection], output: ygg.implement/v1 }\n  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app, max_turns: 2, input: [rejection], output: ygg.implement/v1 }\n  - { id: c, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
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

func TestTheLedgerWarnsOnceWhenACapIsEightyPercentUsed(t *testing.T) {
	script := `steps:
  a: [ { result: { status: ok, usage: { in: 70, out: 10, turns: 1 } } } ]
  b: [ { result: { status: ok, usage: { in: 5, out: 0, turns: 1 } } } ]
  c: [ { result: { status: ok, usage: { in: 4, out: 0, turns: 1 } } } ]
`
	nodes := twoNodes + "  - { id: c, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	r := startWith(t, setup{nodes: nodes, script: script, budget: caps(100, 100, 200000, 200)})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	text := ledgerText(t, r)
	if n := strings.Count(text, `"event":"budget_warning"`); n != 1 || !strings.Contains(text, "per-run tokens cap is 80% used: 80 of 100") {
		t.Errorf("want one warning, at the session that took the tokens to 80 of 100 (%d found):\n%s", n, text)
	}
	if !r.State.Warned["run/tokens"] {
		t.Errorf("warned = %v; the warning must be remembered, so a resume doesn't repeat it", r.State.Warned)
	}
}

// journalBreaker runs its agent, and then puts a file where the journal's folder, not made yet, would
// be, as a disk that stopped taking writes would: the session's spend can't be appended.
type journalBreaker struct {
	gungnir.Agent
	dir string
}

func (b journalBreaker) Run(ctx context.Context, t gungnir.Task) (gungnir.Result, error) {
	res, err := b.Agent.Run(ctx, t)
	return res, errors.Join(err, os.WriteFile(b.dir, nil, 0o600))
}

// Review I4: a session whose spend the journal can't take stops the run at budget-cap, since the
// per-day cap can't be kept. The spend still counts in the run and stays pending in run.json, the
// leaf is put back to its checkpoint with what the session wrote set aside, and once the journal is
// fixed, approving runs the node again and the journal gets the first session's spend.
func TestASpendTheJournalCannotTakeStopsAtBudgetCapWithTheWorkSetAside(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	journal := filepath.Join(t.TempDir(), "spend")
	again := "    - actions: [ { write: { path: a.txt, content: \"a again\\n\" } } ]\n      result: { status: ok, summary: did a again, usage: { in: 5, out: 1, turns: 1 } }\n"
	script := strings.Replace(spendScript, "  b:\n", again+"  b:\n", 1)
	r := startWith(t, setup{nodes: twoNodes, script: script, journal: journal})
	r.Setup.Agents["planner"] = journalBreaker{Agent: r.Setup.Agents["planner"], dir: journal}
	r.Saved = []string{"repos/app: an earlier discard in this process"} // not this stop's: the gate must not list it
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap || out.Gate != "a" {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at budget-cap at node a", out, err, r.State.PendingGate)
	}
	s, err := run.LoadState(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Usage.InTok != 100 || s.Usage.Turns != 3 || len(s.PendingSpend) != 1 {
		t.Errorf("run.json usage = %+v, pending = %+v; the session's spend counts though the journal didn't take it", s.Usage, s.PendingSpend)
	}
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	head, _ := l.Head(context.Background())
	st, _ := l.Git(context.Background(), "status", "--porcelain")
	if head != s.Leaves["repos/app"].Checkpoint || st != "" {
		t.Errorf("the leaf is at %s with changes %q; want it back at its checkpoint %s", head, st, s.Leaves["repos/app"].Checkpoint)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "a-1.md"))
	for _, want := range []string{journal, "ygg doctor checks only today's journal file", "run.json", "refs/ygg/" + s.ID + "/discarded/"} {
		if !strings.Contains(string(gate), want) {
			t.Errorf("the gate file doesn't name %q:\n%s", want, gate)
		}
	}
	for _, wrong := range []string{"Raise the cap", "wait until", "an earlier discard"} {
		if strings.Contains(string(gate), wrong) {
			t.Errorf("the gate file says %q, which doesn't help a journal that can't be written:\n%s", wrong, gate)
		}
	}
	// The operator fixes the journal and approves: the run goes on.
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	r = reopen(t, r)
	if err := Approve(context.Background(), r, "a", "the journal is fixed"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if r.State.Attempts["a"] != 2 {
		t.Errorf("node a ran %d session(s), want 2: approving runs it again", r.State.Attempts["a"])
	}
	if got := today(t, r); got != r.State.Usage || got.InTok != 100+5+10 {
		t.Errorf("the day's journal = %+v, run usage = %+v; want every session's spend, the first one's too", got, r.State.Usage)
	}
}

// countingAgent counts the sessions its agent was asked to run.
type countingAgent struct {
	gungnir.Agent
	calls *int
}

func (c countingAgent) Run(ctx context.Context, t gungnir.Task) (gungnir.Result, error) {
	*c.calls++
	return c.Agent.Run(ctx, t)
}

// Review M3: another run can spend the day's last turns after the meter let a session start. The
// session must not then start with no turn bound, which a MaxTurns of 0 would mean.
func TestASessionTheCapsLeaveNoTurnsForDoesNotStart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r := startWith(t, setup{nodes: twoNodes, script: spendScript, budget: caps(100000, 100, 200000, 5)})
	var calls int
	r.Setup.Agents["planner"] = countingAgent{Agent: r.Setup.Agents["planner"], calls: &calls}
	// The meter has let the session start; then another run spends the day's last turns.
	spend := func() error {
		return r.Setup.Journal.Append(budget.Entry{Run: "20261016-090000-aaaaaa", Project: "/another/project", Node: "x", Usage: gungnir.Usage{InTok: 10, OutTok: 1, Turns: 5}})
	}
	_, _, err := session(context.Background(), r, sessionSpec{key: "a", role: "planner", repo: "repos/app", text: "Do the work.", started: spend})
	if calls != 0 {
		t.Errorf("the agent ran %d session(s) though the caps left no turns", calls)
	}
	c, ok := jormungandr.IsCap(err)
	if !ok || c.Dimension != gungnir.CapTurns || c.Scope != gungnir.CapDay || c.Used != 5 || c.Limit != 5 {
		t.Fatalf("session() error = %v (%+v); want the per-day turns cap, 5 used of 5", err, c)
	}
	if r.State.Attempts["a"] != 0 {
		t.Errorf("attempts = %d; a session that never started counts none", r.State.Attempts["a"])
	}
	if strings.Contains(ledgerText(t, r), `"event":"node_started"`) {
		t.Errorf("the ledger says a session started that never did:\n%s", ledgerText(t, r))
	}
}
