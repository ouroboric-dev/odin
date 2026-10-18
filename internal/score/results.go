// Package score turns the results of a hidden suite into score.json (spec §11, change benchmark-split).
// It reads the Playwright report the suite wrote, joins it with the suite's manifest by scenario id,
// and adds up the reward. A scenario on one side only is an error, never a pass. It writes score.json
// to the run folder and nowhere else: the score is for the operator and the ledger, never for an agent.
package score

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
)

// maxResults is the most bytes of a results file that are read.
const maxResults = 32 << 20

// MismatchError says the manifest and the results do not name the same scenarios, or that the results
// name one twice. No score comes out of it: a missing scenario can't be told from a failed one.
type MismatchError struct {
	NotRun   []string // in the manifest, not in the results
	Unknown  []string // in the results, not in the manifest
	Repeated []string // a spec title used more than once in the results
}

func (e *MismatchError) Error() string {
	var parts []string
	for _, p := range []struct {
		what string
		ids  []string
	}{{"in the manifest and not in the results", e.NotRun}, {"in the results and not in the manifest", e.Unknown}, {"used twice in the results", e.Repeated}} {
		if len(p.ids) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s: %s", len(p.ids), p.what, list(p.ids)))
		}
	}
	return "the suite's manifest and its results disagree: " + strings.Join(parts, "; ")
}

// list names at most five ids, so an error stays one line.
func list(ids []string) string {
	q := make([]string, 0, 6)
	for i, id := range ids {
		if i == 5 {
			q = append(q, "...")
			break
		}
		q = append(q, fmt.Sprintf("%q", id))
	}
	return strings.Join(q, ", ")
}

// The part of Playwright's JSON reporter format that scoring reads. Suites nest.
type report struct {
	Suites []reportSuite `json:"suites"`
}

type reportSuite struct {
	Specs  []reportSpec  `json:"specs"`
	Suites []reportSuite `json:"suites"`
}

type reportSpec struct {
	Title string `json:"title"`
	Tests []struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	} `json:"tests"`
}

// ReadResults reads a Playwright JSON report and returns the status of each spec by title. A test is
// as good as its last try (a retry that passes counts); a spec passes when all its tests pass, fails
// when one fails, times out or is interrupted, and is skipped when none ran. A report with no spec,
// or one that names a title twice, is an error.
func ReadResults(path string) (map[string]gungnir.ScenarioStatus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the suite wrote no results: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResults+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResults {
		return nil, fmt.Errorf("the results are over %d bytes", maxResults)
	}
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("the results are not a Playwright report: %w", err)
	}
	out := map[string]gungnir.ScenarioStatus{}
	var again []string
	var walk func(ss []reportSuite)
	walk = func(ss []reportSuite) {
		for _, s := range ss {
			for _, sp := range s.Specs {
				if _, twice := out[sp.Title]; twice {
					again = append(again, sp.Title)
				}
				out[sp.Title] = specStatus(sp)
			}
			walk(s.Suites)
		}
	}
	walk(r.Suites)
	if len(again) > 0 {
		slices.Sort(again)
		return nil, &MismatchError{Repeated: slices.Compact(again)}
	}
	if len(out) == 0 {
		return nil, errors.New("the results hold no spec")
	}
	return out, nil
}

func specStatus(sp reportSpec) gungnir.ScenarioStatus {
	ran, skipped := 0, 0
	for _, t := range sp.Tests {
		if len(t.Results) == 0 {
			skipped++
			continue
		}
		switch t.Results[len(t.Results)-1].Status {
		case "passed":
			ran++
		case "skipped":
			skipped++
		default: // failed, timedOut, interrupted, or a word we don't know: not a pass
			return gungnir.ScenarioFailed
		}
	}
	if ran == 0 {
		return gungnir.ScenarioSkipped
	}
	if skipped > 0 {
		return gungnir.ScenarioSkipped
	}
	return gungnir.ScenarioPassed
}
