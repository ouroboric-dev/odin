package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// variant is sample() as another run: a later id, a changed factory and one role on another model, cheaper,
// with fewer interventions and the same reward.
func variant() *Run {
	r := sample()
	r.ID = "20261024-090000-d4e5f6"
	r.Factory = []string{"odin 0123456789ab gungnir 32cc95d9d0ff"}
	r.ConfigSHA256 = strings.Repeat("d1", 32)
	r.Roles["reviewer"] = Setting{"opencode", "openrouter/qwen/qwen3.9-30b"}
	r.Usage.CostUSD, r.Usage.BilledUSD, r.Usage.Turns = 1.2, 0.2, 380
	r.Interventions = Interventions{Rejections: 0, NotedApprovals: 1, Escalations: 1}
	r.Loops.FirstPass = Ratio{3, 4}
	r.Review.Vetoes = Ratio{1, 9} // fewer vetoes: a reviewer who is satisfied, or one who sees less; the metric has no direction
	r.Time.Active = Duration(50 * time.Minute)
	return r
}

func TestCompareSaysWhatDifferedBetweenTheTwoSetups(t *testing.T) {
	c := Compare(sample(), variant())
	got := map[string][2]string{}
	for _, d := range c.Differences {
		got[d.What] = [2]string{d.A, d.B}
	}
	for _, what := range []string{"factory", "config", "role reviewer"} {
		if _, ok := got[what]; !ok {
			t.Errorf("Differences lacks %q: %+v", what, c.Differences)
		}
	}
	if d := got["role reviewer"]; d[0] != "opencode openrouter/qwen/qwen3.8-27b" || d[1] != "opencode openrouter/qwen/qwen3.9-30b" {
		t.Errorf("role reviewer = %v", d)
	}
	for _, same := range []string{"mode", "graph", "brief", "role implementer", "suite"} {
		if _, ok := got[same]; ok {
			t.Errorf("Differences names %q, which did not differ", same)
		}
	}
}

func TestCompareDeltasAreBMinusAAndNameWhatImprovedAndWhatGotWorse(t *testing.T) {
	c := Compare(sample(), variant())
	rows := map[string]Row{}
	for _, r := range c.Rows {
		rows[r.Name] = r
	}
	if r := rows["cost at API prices (usd)"]; !r.Valid || !near(r.Delta(), -0.3) || r.Better != Lower {
		t.Errorf("cost row = %+v (delta %v)", r, r.Delta())
	}
	if r := rows["billed (usd)"]; !r.Valid || !near(r.A, 0.3) || !near(r.B, 0.2) {
		t.Errorf("billed row = %+v", r)
	}
	if r := rows["first pass (%)"]; !near(r.A, 50) || !near(r.B, 75) || !near(r.Delta(), 25) {
		t.Errorf("first pass row = %+v", r)
	}
	if r := rows["interventions"]; !near(r.Delta(), -2) {
		t.Errorf("interventions row = %+v", r)
	}
	if r := rows["cost per task (usd)"]; !r.Valid || !near(r.A, 0.75) || !near(r.B, 0.6) {
		t.Errorf("cost per task = %+v; want 1.5/2 done tasks... and 1.2/2", r)
	}
	v := c.Verdict()
	for _, want := range []string{"first pass (%)", "cost at API prices (usd)", "interventions", "cost per task (usd)"} {
		if !contains(v.Improved, want) {
			t.Errorf("Improved = %v, want it to hold %q", v.Improved, want)
		}
	}
	if contains(v.Improved, "reviewer veto (%)") || contains(v.Worse, "reviewer veto (%)") {
		t.Errorf("the veto rate has no direction, yet the verdict judged it: %+v", v)
	}
	if len(v.Worse) != 0 || !v.RewardHeld {
		t.Errorf("Verdict = %+v; want nothing worse and the reward held", v)
	}
}

// Spec §2.3 criterion 7: a change counts only if the hidden score did not fall.
func TestAFallingRewardIsNeverHiddenBehindAnImprovedMetric(t *testing.T) {
	b := variant()
	b.Grade.Reward = 0.5
	v := Compare(sample(), b).Verdict()
	if v.RewardHeld || !contains(v.Worse, "reward") {
		t.Errorf("Verdict = %+v; want the reward named as worse and not held", v)
	}
	b = variant()
	b.Grade = nil
	v = Compare(sample(), b).Verdict()
	if v.RewardHeld || v.RewardKnown {
		t.Errorf("Verdict = %+v; a run with no grade cannot show that the reward held", v)
	}
}

func TestRunsOfAnotherBriefOrSuiteAreNotComparableAndTheReportSaysWhy(t *testing.T) {
	b := variant()
	b.BriefSHA256 = strings.Repeat("e2", 32)
	b.Grade.Suite = "v3"
	c := Compare(sample(), b)
	if c.Comparable || len(c.Reasons) != 2 {
		t.Errorf("Comparable = %v, Reasons = %v; want two reasons (brief and suite)", c.Comparable, c.Reasons)
	}
	var w bytes.Buffer
	c.WriteText(&w)
	if out := w.String(); !strings.Contains(out, "comparable:   NO") || !strings.Contains(out, "the briefs differ") || !strings.Contains(out, "the suites differ") {
		t.Errorf("the text does not warn:\n%s", out)
	}
	if !Compare(sample(), variant()).Comparable {
		t.Error("two runs of the same brief and suite must be comparable")
	}
}

func TestCompareTextHasTheDifferencesTheTableAndTheVerdict(t *testing.T) {
	var w bytes.Buffer
	Compare(sample(), variant()).WriteText(&w)
	out := w.String()
	for _, want := range []string{"compare:", "20261023-090000-a1b2c3", "20261024-090000-d4e5f6", "comparable:   yes",
		"what differed:", "role reviewer", "qwen3.9-30b", "metric", "cost at API prices (usd)", "-0.30", "first pass (%)", "+25",
		"improved:", "worse:        none", "reward:       held (0.7125 to 0.7125)", "0.7125"} {
		if !strings.Contains(out, want) {
			t.Errorf("the text lacks %q:\n%s", want, out)
		}
	}
}

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

// A comparison across modes is not comparable: interventions and the score are not measured the same way.
func TestAComparisonAcrossModesIsNotComparable(t *testing.T) {
	b := variant()
	b.Mode = "unattended"
	c := Compare(sample(), b)
	if c.Comparable {
		t.Fatalf("Comparable = true for two modes; want false")
	}
	if got := strings.Join(c.Reasons, "; "); !strings.Contains(got, "the modes differ (key-points and unattended): interventions and the score are not measured the same way") {
		t.Errorf("Reasons = %q; want the mode reason", got)
	}
}

// A pair that is not comparable keeps its header and rows, and gets no verdict.
func TestNotComparableTextHasNoVerdict(t *testing.T) {
	b := variant()
	b.BriefSHA256 = strings.Repeat("e2", 32)
	var w bytes.Buffer
	Compare(sample(), b).WriteText(&w)
	out := w.String()
	if !strings.Contains(out, "comparable:   NO") || !strings.Contains(out, "first pass (%)") || !strings.Contains(out, "cost at API prices (usd)") {
		t.Errorf("the text lacks the header or the rows:\n%s", out)
	}
	if !strings.Contains(out, "verdict: not judged: the runs are not comparable") {
		t.Errorf("the text lacks the no-verdict line:\n%s", out)
	}
	for _, banned := range []string{"improved:", "held"} {
		if strings.Contains(out, banned) {
			t.Errorf("the text of a not-comparable pair contains %q:\n%s", banned, out)
		}
	}
}
