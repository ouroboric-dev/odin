package budget

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

func meter(t *testing.T, run *gungnir.Usage, usd bool) (*Meter, *Journal) {
	j := journal(t, noon)
	return &Meter{Run: run, Journal: j, USD: usd, Caps: schema.Budget{
		PerRun: schema.Cap{Tokens: 1000, Turns: 10, USD: 5}, PerDay: schema.Cap{Tokens: 3000, Turns: 30, USD: 20}}}, j
}

func TestTheMeterStopsAtTheRunsCapThenTheDays(t *testing.T) {
	run := &gungnir.Usage{}
	m, j := meter(t, run, false)
	if hit := m.Check(); hit != nil {
		t.Fatalf("Check() = %v on an empty run", hit)
	}
	run.InTok = 1000
	if hit := m.Check(); hit == nil || hit.Scope != gungnir.CapRun || hit.Dimension != gungnir.CapTokens {
		t.Errorf("Check() = %v, want the run's token cap", hit)
	}
	run.InTok = 10
	j.Append(entry("20261016-090000-aaaaaa", 2999, 5)) // other runs spent the day's tokens: 2999 and 1 out
	if hit := m.Check(); hit == nil || hit.Scope != gungnir.CapDay || hit.Dimension != gungnir.CapTokens {
		t.Errorf("Check() = %v, want the day's token cap, reached by other runs", hit)
	}
	m.Journal = journal(t, noon.Add(24*time.Hour)) // tomorrow
	if hit := m.Check(); hit != nil {
		t.Errorf("Check() = %v on a new day", hit)
	}
}

func TestTheDollarCapCountsOnlyForAPaidAPI(t *testing.T) {
	run := &gungnir.Usage{CostUSD: 5}
	m, _ := meter(t, run, false)
	if hit := m.Check(); hit != nil {
		t.Errorf("Check() = %v; a subscription's dollars are only a figure", hit)
	}
	m.USD = true
	if hit := m.Check(); hit == nil || hit.Dimension != gungnir.CapUSD {
		t.Errorf("Check() = %v, want the dollar cap", hit)
	}
}

func TestAJournalThatCannotBeReadStopsTheRunAndSaysWhy(t *testing.T) {
	m, j := meter(t, &gungnir.Usage{}, false)
	os.MkdirAll(j.Dir, 0o700)
	os.WriteFile(j.File(noon), []byte("garbage\nmore garbage\n"), 0o600)
	hit := m.Check()
	if hit == nil || hit.Dimension != gungnir.CapJournal || !strings.Contains(hit.Error(), "2026-10-16.jsonl:1") {
		t.Errorf("Check() = %v; an unreadable journal must count as a reached cap", hit)
	}
	if m.RemainingTurns() != 0 {
		t.Errorf("RemainingTurns() = %d, want 0", m.RemainingTurns())
	}
}

func TestTheTurnsLeftAreTheLesserOfTheRunsAndTheDays(t *testing.T) {
	run := &gungnir.Usage{Turns: 4}
	m, j := meter(t, run, false)
	if got := m.RemainingTurns(); got != 6 {
		t.Errorf("RemainingTurns() = %d, want 6 (the run's)", got)
	}
	j.Append(entry("20261016-090000-aaaaaa", 1, 27))
	if got := m.RemainingTurns(); got != 3 {
		t.Errorf("RemainingTurns() = %d, want 3 (the day's)", got)
	}
	run.Turns = 99
	if got := m.RemainingTurns(); got != 0 {
		t.Errorf("RemainingTurns() = %d, want 0, never negative", got)
	}
}

func TestNearListsTheCapsOverEightyPercentAndNotYetReached(t *testing.T) {
	run := &gungnir.Usage{InTok: 850, Turns: 10, CostUSD: 4.5}
	m, _ := meter(t, run, true)
	var got []string
	for _, n := range m.Near(0.8) {
		got = append(got, string(n.Scope)+"/"+string(n.Dimension))
	}
	if strings.Join(got, ",") != "run/tokens,run/usd" {
		t.Errorf("Near() = %v; turns is reached, not near, and the day's caps are far", got)
	}
	m.USD = false
	if n := m.Near(0.8); len(n) != 1 || n[0].Dimension != gungnir.CapTokens {
		t.Errorf("Near() = %+v without dollars", n)
	}
}
