package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
)

const testRun = "20261023-090000-a1b2c3"

var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// fake is a run folder under construction: events go through the real ledger writer, so the chain is real.
type fake struct {
	t     *testing.T
	dir   string
	l     *ledger.Ledger
	State *run.State
}

func newFake(t *testing.T) *fake {
	t.Helper()
	dir := filepath.Join(t.TempDir(), testRun)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), ledger.Anchor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &fake{t: t, dir: dir, l: l, State: &run.State{ID: testRun, Status: run.StatusDone, Mode: gungnir.ModeKeyPoints,
		Graph: "software", GraphVersion: "0.1.0", ConfigSHA256: "c0", BriefSHA256: "b0",
		Roles:  map[string]run.RoleRecord{"implementer": {Agent: "claude-code", Model: "sonnet"}, "reviewer": {Agent: "opencode", Model: "openrouter/qwen/qwen3.8-27b"}},
		Realms: map[string]run.RealmRecord{"helheim-go": {Version: "0.1.0"}}}}
}

// log appends e at minute min after the run began.
func (f *fake) log(min float64, e gungnir.Event) *fake {
	f.t.Helper()
	e.Run, e.Time = testRun, t0.Add(time.Duration(min*float64(time.Minute)))
	if err := f.l.Append(e); err != nil {
		f.t.Fatal(err)
	}
	return f
}

// session is a finished agent session with usage.
func session(node, role, adapter, model string, in, out, turns int64, cost float64, estimated bool) gungnir.Event {
	return gungnir.Event{Name: gungnir.EventNodeFinished, Node: node, Attempt: 1, Role: role, Adapter: adapter, Model: model,
		InputTokens: in, OutputTokens: out, CacheReadTokens: in * 2, Turns: turns, CostUSD: cost, CostEstimated: estimated}
}

// dir saves run.json with the ledger's anchor and returns the run folder.
func (f *fake) done() string {
	f.t.Helper()
	f.State.Ledger = f.l.Head()
	if err := f.State.Save(f.dir); err != nil {
		f.t.Fatal(err)
	}
	return f.dir
}

func (f *fake) features(fs ...schema.FeatureRecord) *fake {
	f.State.Features = fs
	return f
}

func feature(id, leaf string, status schema.TaskStatus, why string) schema.FeatureRecord {
	return schema.FeatureRecord{Feature: schema.Feature{ID: id, Title: "title of " + id, Layer: leaf}, Status: status, Why: why}
}

func mustCollect(t *testing.T, dir string) *Run {
	t.Helper()
	r, err := Collect(dir)
	if err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	return r
}
