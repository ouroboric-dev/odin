package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

// maxLine bounds one ledger line; the ledger schema keeps lines far below it.
const maxLine = 4 << 20

// Collect reads the run in dir (a folder of .factory/runs) and measures it. The ledger's hash chain is checked
// first, against the anchor in run.json: a ledger that was changed or cut short is an error and gives no numbers.
func Collect(dir string) (*Run, error) {
	state, err := loadRecord(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "ledger.jsonl")
	if _, err := ledger.Verify(path, state.Ledger); err != nil {
		return nil, fmt.Errorf("the ledger of run %s can't be trusted: %w", state.ID, err)
	}
	events, err := readEvents(path)
	if err != nil {
		return nil, err
	}
	r := &Run{ID: state.ID, Status: string(state.Status), Mode: string(state.Mode), Graph: state.Graph, GraphVersion: state.GraphVersion,
		ConfigSHA256: state.ConfigSHA256, BriefSHA256: state.BriefSHA256, Roles: map[string]Setting{}, Realms: map[string]string{},
		ByRole: map[string]Usage{}, Factory: []string{}}
	for name, rr := range state.Roles {
		r.Roles[name] = Setting{Agent: rr.Agent, Model: rr.Model}
	}
	for name, rl := range state.Realms {
		r.Realms[name] = rl.Version
	}
	if len(events) == 0 {
		return r, nil
	}
	taskUsage := map[string]Usage{}
	for _, e := range events {
		switch e.Name {
		case gungnir.EventRunStarted, gungnir.EventRunResumed:
			if e.FactoryVersion != "" && !slices.Contains(r.Factory, e.FactoryVersion) {
				r.Factory = append(r.Factory, e.FactoryVersion)
			}
		case gungnir.EventRetry:
			r.Retries++
		}
		u, ok := sessionUsage(e)
		if !ok {
			continue
		}
		r.Usage.add(u)
		key := e.Role + " " + e.Adapter + " " + e.Model
		by := r.ByRole[key]
		by.add(u)
		r.ByRole[key] = by
		if e.Task != "" {
			tu := taskUsage[e.Task]
			tu.add(u)
			taskUsage[e.Task] = tu
		}
	}
	return r, nil
}

func readEvents(path string) ([]gungnir.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	var out []gungnir.Event
	for n := 1; sc.Scan(); n++ {
		var e gungnir.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// sessionUsage is the usage of a ledger line that records one session. The run's own totals (run_finished,
// run_paused, run_failed) repeat the sessions and are left out.
func sessionUsage(e gungnir.Event) (Usage, bool) {
	switch e.Name {
	case gungnir.EventRunFinished, gungnir.EventRunPaused, gungnir.EventRunFailed:
		return Usage{}, false
	}
	if e.InputTokens == 0 && e.OutputTokens == 0 && e.Turns == 0 && e.CostUSD == 0 {
		return Usage{}, false
	}
	u := Usage{InTok: e.InputTokens, OutTok: e.OutputTokens, CacheTok: e.CacheReadTokens, Turns: e.Turns, CostUSD: e.CostUSD, Sessions: 1}
	if e.CostEstimated {
		u.EstimatedUSD = e.CostUSD
	}
	if schema.PaidAPI(e.Adapter) {
		u.BilledUSD = e.CostUSD
	}
	if e.CostUSD == 0 && (e.InputTokens > 0 || e.OutputTokens > 0) && e.Adapter != "scripted" {
		u.Unpriced = 1
	}
	return u, true
}
