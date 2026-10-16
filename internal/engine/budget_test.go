package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
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
