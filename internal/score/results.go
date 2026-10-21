// Package score turns the results of a hidden suite into score.json (spec §11, change benchmark-split).
// It reads the Playwright report the suite printed on its stdout, joins it with the suite's manifest by scenario id,
// and adds up the reward. A scenario on one side only is an error, never a pass. It writes score.json
// to the run folder and nowhere else: the score is for the operator and the ledger, never for an agent.
package score

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
)

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

// marker starts the line a suite prints for Odin: marker, a space, the key, a space, the report on one line.
const marker = "ygg-results/v1"

// ParseStdout finds the report the suite printed on its stdout, on the one line that starts with the marker
// and carries key, and returns the status of each spec by title. Every other line is the suite's or the
// app's own output and is ignored, a line of another key too. A test is as good as its last try (a retry
// that passes counts); a spec passes when all its tests pass, fails when one fails, times out or is
// interrupted, and is skipped when none ran. No keyed line, two or more of them (identical or not), a line
// that is not a report, a report with no spec, or one that names a title twice, is an error. The line is
// found by searching the bytes, so its length is the stdout's bound and no scanner's.
func ParseStdout(stdout []byte, key string) (map[string]gungnir.ScenarioStatus, error) {
	prefix := []byte(marker + " " + key)
	var found []byte
	n := 0
	for rest := stdout; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if after, ok := bytes.CutPrefix(line, prefix); ok && (len(after) == 0 || after[0] == ' ') {
			found = after
			n++
		}
	}
	switch {
	case n == 0:
		return nil, errors.New("the suite printed no scores: no line on its stdout starts with " + marker + " and the key")
	case n > 1:
		return nil, fmt.Errorf("the suite printed the scores twice: %d lines on its stdout carry the key, and none is taken", n)
	}
	b := found
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("the line with the key is not a Playwright report: %w", err)
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
		return nil, errors.New("the report holds no spec")
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
