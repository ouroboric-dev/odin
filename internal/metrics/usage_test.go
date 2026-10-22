package metrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsageIsTheSumOfTheSessionLinesAndNeverTheRunTotalsToo(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted, FactoryVersion: "odin aaaaaaaaaaaa"})
	f.log(5, session("implement", "implementer", "claude-code", "sonnet", 1000, 200, 4, 0.08, true))
	f.log(9, session("implement-quality", "reviewer", "opencode", "openrouter/qwen/qwen3.8-27b", 500, 50, 2, 0.012, false))
	failed := session("implement", "implementer", "claude-code", "sonnet", 300, 30, 1, 0.02, true)
	failed.Name = gungnir.EventNodeFailed
	f.log(12, failed)
	// The run's own totals repeat the sessions: counting them would double everything.
	total := gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"}.WithUsage(gungnir.Usage{InTok: 1800, OutTok: 280, CacheTok: 3600, Turns: 7, CostUSD: 0.112, CostEstimated: true})
	f.log(15, total)
	r := mustCollect(t, f.done())
	u := r.Usage
	if u.InTok != 1800 || u.OutTok != 280 || u.CacheTok != 3600 || u.Turns != 7 || u.Sessions != 3 {
		t.Errorf("usage = %+v; want 1800 in, 280 out, 3600 cache, 7 turns, 3 sessions", u)
	}
	if !near(u.CostUSD, 0.112) || !near(u.EstimatedUSD, 0.10) {
		t.Errorf("cost = %v, estimated = %v; want 0.112 and 0.10", u.CostUSD, u.EstimatedUSD)
	}
	// Only the OpenCode session was billed; the Claude Code sessions are a subscription's, at API prices.
	if !near(u.BilledUSD, 0.012) {
		t.Errorf("billed = %v; want 0.012", u.BilledUSD)
	}
	impl := r.ByRole["implementer claude-code sonnet"]
	if impl.Sessions != 2 || impl.Turns != 5 || !near(impl.CostUSD, 0.10) {
		t.Errorf("by role implementer = %+v", impl)
	}
	if got := r.ByRole["reviewer opencode openrouter/qwen/qwen3.8-27b"]; got.Sessions != 1 {
		t.Errorf("by role reviewer = %+v", got)
	}
	if want := []string{"odin aaaaaaaaaaaa"}; !equal(r.Factory, want) {
		t.Errorf("Factory = %v, want %v", r.Factory, want)
	}
}

func equal(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

// B-107: a session with tokens and no cost is unknown, not free.
func TestASessionWithTokensAndNoCostIsCountedAsUnpricedNotAsFree(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(3, session("implement-quality", "reviewer", "opencode", "openrouter/anthropic/claude-haiku-5-5", 900, 90, 2, 0, false))
	f.log(4, session("implement-spec", "reviewer", "opencode", "openrouter/qwen/qwen3.8-27b", 900, 90, 2, 0.01, false))
	r := mustCollect(t, f.done())
	if r.Usage.Unpriced != 1 || !near(r.Usage.CostUSD, 0.01) {
		t.Errorf("Unpriced = %d, cost = %v; want 1 and 0.01", r.Usage.Unpriced, r.Usage.CostUSD)
	}
}

func TestACutOrChangedLedgerIsRefusedBecauseNoNumberFromItCanBeTrusted(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, session("a", "implementer", "claude-code", "sonnet", 10, 1, 1, 0.01, true))
	dir := f.done()
	path := filepath.Join(dir, "ledger.jsonl")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), `"ygg.usage.cost_usd":0.01`, `"ygg.usage.cost_usd":0.00`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(dir); err == nil || !strings.Contains(err.Error(), "ledger.jsonl") {
		t.Errorf("Collect() = %v, want a refusal that names the ledger", err)
	}
}

// Compare reads runs of different builds. A build that dropped or added a field of run.json must not make an
// older run unreadable: the record is read for the few fields metrics uses and nothing else.
func TestARunWrittenByAnotherBuildWithOtherFieldsIsStillMeasured(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, session("a", "implementer", "claude-code", "sonnet", 10, 1, 1, 0.01, true))
	dir := f.done()
	b, _ := os.ReadFile(filepath.Join(dir, "run.json"))
	b = []byte(strings.Replace(strings.Replace(string(b), `"schema": "ygg.run/v3"`, `"schema": "ygg.run/v2", "lead_realm": "midgard-react", "from_the_future": 7`, 1), `"id":`, `"extra": {"a": 1}, "id":`, 1))
	if err := os.WriteFile(filepath.Join(dir, "run.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	r := mustCollect(t, dir)
	if r.Usage.Sessions != 1 || r.Graph != "software" {
		t.Errorf("Run = %+v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"schema": "something/else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(dir); err == nil {
		t.Error("Collect() accepted a run.json that is not a ygg run record")
	}
}

func TestUsageThatDisagreesWithRunJSONIsReportedAsAWarning(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, session("a", "implementer", "claude-code", "sonnet", 10, 1, 1, 0.01, true))
	f.State.Usage = gungnir.Usage{InTok: 10, OutTok: 1, CacheTok: 20, Turns: 1, CostUSD: 0.01}
	if r := mustCollect(t, f.done()); len(r.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none when the ledger and run.json agree", r.Warnings)
	}
	g := newFake(t)
	g.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, session("a", "implementer", "claude-code", "sonnet", 10, 1, 1, 0.01, true))
	g.State.Usage = gungnir.Usage{InTok: 99, OutTok: 1, Turns: 1, CostUSD: 0.01}
	r := mustCollect(t, g.done())
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "run.json") {
		t.Errorf("Warnings = %v, want one that names run.json", r.Warnings)
	}
}

func TestSessionsOfTheScriptedAdapterAreNotUnpriced(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, session("a", "implementer", "scripted", "none", 100, 10, 2, 0, false))
	if r := mustCollect(t, f.done()); r.Usage.Unpriced != 0 || r.Usage.Sessions != 1 {
		t.Errorf("Usage = %+v; a scripted session has no price to miss", r.Usage)
	}
}
