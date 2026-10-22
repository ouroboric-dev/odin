package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func sample() *Run {
	bench, base, bar := 0.9, 0.5, 0.8
	return &Run{
		ID: "20261023-090000-a1b2c3", Status: "done", Mode: "key-points", Graph: "software", GraphVersion: "0.1.0",
		Factory: []string{"odin b8c9716fa818 gungnir 32cc95d9d0ff"}, ConfigSHA256: strings.Repeat("c0", 32), BriefSHA256: strings.Repeat("b0", 32),
		Roles:         map[string]Setting{"implementer": {"claude-code", "sonnet"}, "reviewer": {"opencode", "openrouter/qwen/qwen3.8-27b"}},
		Realms:        map[string]string{"helheim-go": "0.1.0"},
		Time:          Time{Elapsed: Duration(2*time.Hour + 5*time.Minute), Active: Duration(65 * time.Minute), ApprovalWait: Duration(50 * time.Minute), Stopped: Duration(10 * time.Minute)},
		Usage:         Usage{InTok: 1234567, OutTok: 89012, CacheTok: 3000000, Turns: 410, CostUSD: 1.5, EstimatedUSD: 1.2, BilledUSD: 0.3, Sessions: 90, Unpriced: 3},
		ByRole:        map[string]Usage{"implementer claude-code sonnet": {Turns: 300, CostUSD: 1.2, EstimatedUSD: 1.2, Sessions: 30}},
		Tasks:         []Task{{ID: "g-T1", Leaf: "repos/api", Status: "done", Usage: Usage{Turns: 12, CostUSD: 0.2}}, {ID: "g-T2", Leaf: "repos/web", Status: "blocked", Why: "attempts-exhausted"}},
		Leaves:        map[string]Group{"repos/api": {Tasks: 1, Done: 1}, "repos/web": {Tasks: 1, Blocked: 1}},
		BlockedTasks:  []BlockedTask{{Task: "g-T2", Why: "attempts-exhausted"}},
		Loops:         Loops{Finished: 4, Passed: 3, GaveUp: 1, FirstPass: Ratio{2, 4}, AvgAttempts: 1.75},
		Review:        Review{Vetoes: Ratio{3, 9}},
		Findings:      Findings{RaisedByCheck: 2, RaisedByReview: 5, Closed: 6, Overruled: 1, DisputesIgnored: 1},
		Interventions: Interventions{Rejections: 1, NotedApprovals: 2, Escalations: 1},
		Grade: &Grade{Task: "client-gallery-lite", Suite: "v2", Reward: 0.7125, FailToPass: Tally{20, 29}, PassToPass: Tally{4, 4},
			Categories: map[string]Category{"security": {Passed: 5, Total: 8, Weight: 0.35}}, EscapedDefects: 9, TasksGivenUp: 1},
		Routing:  Routing{Indexes: 2, Bench: &bench, Baseline: &base, Threshold: &bar},
		Warnings: []string{"the ledger's sessions add up to 1 tokens, but run.json says 2"},
	}
}

func TestTheTextReportSaysEachFigureOfSpecSection102InWords(t *testing.T) {
	var b bytes.Buffer
	sample().WriteText(&b)
	out := b.String()
	for _, want := range []string{
		"run:", "20261023-090000-a1b2c3", "(done, key-points, graph software 0.1.0)",
		"factory:      odin b8c9716fa818 gungnir 32cc95d9d0ff",
		"implementer = claude-code sonnet",
		"time:         1h05m active of 2h05m elapsed (approval waits 50m, stopped 10m)",
		"usage:        1,234,567 tokens in, 89,012 out, 3,000,000 from cache; 410 turns; 90 sessions",
		"cost:         $1.5000 at API prices, $0.3000 of it billed to a paid API; 3 sessions had tokens and no cost, so this is a floor",
		"first pass 2 of 4 (50%)", "1.75 attempts on average", "1 gave up",
		"reviewer vetoes 3 of 9 attempts (33%)",
		"raised 7 (2 by checks, 5 by reviewers)", "overruled 1", "disputes ignored 1",
		"interventions: 4 (1 rejections, 2 approvals with notes, 1 escalations)",
		"blocked:      g-T2: attempts-exhausted",
		"reward 0.7125", "fail-to-pass 20/29", "pass-to-pass 4/4", "escaped defects 9 (1 tasks were given up: some failures are missing work)",
		"security 5/8 (weight 0.35)",
		"bench 90% (grep baseline 50%, bar 80%)",
		"warning:      the ledger's sessions add up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
}

func TestTheTextReportNeverShowsAnEmptyRateAsZeroOrAMissingGradeAsAScore(t *testing.T) {
	var b bytes.Buffer
	(&Run{ID: "20261023-090000-a1b2c3", Status: "running", Mode: "unattended", Roles: map[string]Setting{}}).WriteText(&b)
	out := b.String()
	for _, want := range []string{"first pass n/a (no loop has ended)", "reviewer vetoes n/a (no reviewer has run)",
		"grade:        not graded", "factory:      not recorded", "bench not measured"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0%") {
		t.Errorf("a rate over nothing was shown as 0%%:\n%s", out)
	}
}

func TestARangeTableHasARowPerRunAndATotalLine(t *testing.T) {
	a, b := sample(), sample()
	b.ID, b.Grade = "20261023-100000-d4e5f6", nil
	var w bytes.Buffer
	WriteRange(&w, []*Run{a, b})
	out := w.String()
	for _, want := range []string{"20261023-090000-a1b2c3", "20261023-100000-d4e5f6", "total", "0.7125", "not graded", "$3.0000"} {
		if !strings.Contains(out, want) {
			t.Errorf("the range lacks %q:\n%s", want, out)
		}
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines != 3 {
		t.Errorf("the range has %d lines after the header, want 3 (two runs and a total):\n%s", lines, out)
	}
}

// A blocked task's reason can hold text an agent wrote. A terminal acts on control characters, so none is printed.
func TestTextFromAnAgentIsPrintedOnOneLineWithNoControlCharacters(t *testing.T) {
	r := sample()
	r.BlockedTasks = []BlockedTask{{Task: "g-T2", Why: "gave up\n\x1b[31m# injected\x07\u009b​‮ \r\nsecond line"}}
	r.Warnings = []string{"the sessions\nadd up‮ wrong"}
	r.Grade = &Grade{Refused: "no grade\nfor this‮ run", Categories: map[string]Category{}}
	var b bytes.Buffer
	r.WriteText(&b)
	out := b.String()
	if strings.ContainsAny(out, "\x1b\x07\r") {
		t.Errorf("a control character reached the output:\n%q", out)
	}
	for _, bad := range []string{"\u009b", "​", "‮"} {
		if strings.Contains(out, bad) {
			t.Errorf("the character %U reached the output:\n%q", []rune(bad)[0], out)
		}
	}
	if !strings.Contains(out, "g-T2: gave up [31m# injected second line") {
		t.Errorf("the reason is not one clean line:\n%s", out)
	}
	if !strings.Contains(out, "warning:      the sessions add up wrong\n") {
		t.Errorf("the warning is not one clean line:\n%s", out)
	}
	if !strings.Contains(out, "grade:        refused: no grade for this run\n") {
		t.Errorf("the refusal is not one clean line:\n%s", out)
	}
}
