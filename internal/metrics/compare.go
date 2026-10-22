package metrics

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Direction says which way a metric improves.
type Direction int

const (
	Lower   Direction = -1
	Neutral Direction = 0
	Higher  Direction = 1
)

// Row is one metric of two runs. Valid is false when either run has no value for it.
type Row struct {
	Name   string    `json:"name"`
	A      float64   `json:"a"`
	B      float64   `json:"b"`
	D      float64   `json:"delta"` // B minus A
	Valid  bool      `json:"valid"`
	Better Direction `json:"better"`
	Dec    int       `json:"decimals"` // decimals to print: 0 for a count
}

// Delta is B minus A.
func (r Row) Delta() float64 { return r.D }

// Difference is one thing that differed between the two setups.
type Difference struct {
	What string `json:"what"`
	A    string `json:"a"`
	B    string `json:"b"`
}

// Comparison is ygg metrics --compare: what differed, whether the two runs may be compared, and the deltas.
type Comparison struct {
	A           string       `json:"a"`
	B           string       `json:"b"`
	Comparable  bool         `json:"comparable"`
	Reasons     []string     `json:"reasons"`
	Differences []Difference `json:"differences"`
	Rows        []Row        `json:"rows"`

	a, b *Run
}

// Verdict is what the deltas say for success criterion 7 (design doc §2.3): which metrics improved, which got
// worse, and whether the hidden score held. A change improves the factory only if the score held.
type Verdict struct {
	Improved    []string `json:"improved"`
	Worse       []string `json:"worse"`
	RewardKnown bool     `json:"reward_known"`
	RewardHeld  bool     `json:"reward_held"`
}

// Compare lays two runs side by side. Two runs of another brief, task version or suite are not comparable: the
// deltas are still computed, and the result says why they do not mean what a delta usually means.
func Compare(a, b *Run) *Comparison {
	c := &Comparison{A: a.ID, B: b.ID, a: a, b: b, Comparable: true, Reasons: []string{}, Differences: []Difference{}}
	c.differences()
	c.rows()
	return c
}

func (c *Comparison) differ(what, a, b string) {
	if a != b {
		c.Differences = append(c.Differences, Difference{what, a, b})
	}
}

func (c *Comparison) differences() {
	a, b := c.a, c.b
	c.differ("factory", orNone(strings.Join(a.Factory, "; ")), orNone(strings.Join(b.Factory, "; ")))
	c.differ("config", short(a.ConfigSHA256), short(b.ConfigSHA256))
	c.differ("mode", a.Mode, b.Mode)
	c.differ("graph", a.Graph+" "+a.GraphVersion, b.Graph+" "+b.GraphVersion)
	c.differ("brief", short(a.BriefSHA256), short(b.BriefSHA256))
	for _, name := range union(maps.Keys(a.Realms), maps.Keys(b.Realms)) {
		c.differ("realm "+name, orNone(a.Realms[name]), orNone(b.Realms[name]))
	}
	for _, name := range union(maps.Keys(a.Roles), maps.Keys(b.Roles)) {
		c.differ("role "+name, setting(a.Roles, name), setting(b.Roles, name))
	}
	if a.Grade != nil && b.Grade != nil {
		c.differ("task version", a.Grade.TaskVersion, b.Grade.TaskVersion)
		c.differ("suite", a.Grade.Suite, b.Grade.Suite)
		if a.Grade.TaskVersion != b.Grade.TaskVersion {
			c.Comparable = false
			c.Reasons = append(c.Reasons, "the task versions differ ("+a.Grade.TaskVersion+" and "+b.Grade.TaskVersion+")")
		}
		if a.Grade.Suite != b.Grade.Suite {
			c.Comparable = false
			c.Reasons = append(c.Reasons, "the suites differ ("+a.Grade.Suite+" and "+b.Grade.Suite+"): their scores are not on one scale")
		}
	}
	if a.BriefSHA256 != b.BriefSHA256 {
		c.Comparable = false
		c.Reasons = append(c.Reasons, "the briefs differ: these are two different tasks")
	}
}

func (c *Comparison) rows() {
	a, b := c.a, c.b
	add := func(name string, better Direction, dec int, av, bv float64, valid bool) {
		c.Rows = append(c.Rows, Row{Name: name, A: av, B: bv, D: bv - av, Valid: valid, Better: better, Dec: dec})
	}
	both := func(f func(*Run) (float64, bool)) (float64, float64, bool) {
		x, okx := f(a)
		y, oky := f(b)
		return x, y, okx && oky
	}
	num := func(name string, better Direction, dec int, f func(*Run) (float64, bool)) {
		x, y, ok := both(f)
		add(name, better, dec, x, y, ok)
	}
	always := func(f func(*Run) float64) func(*Run) (float64, bool) {
		return func(r *Run) (float64, bool) { return f(r), true }
	}
	num("active time (min)", Lower, 2, always(func(r *Run) float64 { return time.Duration(r.Time.Active).Minutes() }))
	num("cost at API prices (usd)", Lower, 2, always(func(r *Run) float64 { return r.Usage.CostUSD }))
	num("billed (usd)", Lower, 2, always(func(r *Run) float64 { return r.Usage.BilledUSD }))
	num("cost per task (usd)", Lower, 2, func(r *Run) (float64, bool) {
		n := len(r.Tasks)
		if n == 0 {
			n = r.Loops.Finished
		}
		return r.Usage.CostUSD / float64(max(n, 1)), n > 0
	})
	num("turns", Lower, 0, always(func(r *Run) float64 { return float64(r.Usage.Turns) }))
	num("tokens in", Lower, 0, always(func(r *Run) float64 { return float64(r.Usage.InTok) }))
	num("tokens out", Lower, 0, always(func(r *Run) float64 { return float64(r.Usage.OutTok) }))
	num("first pass (%)", Higher, 2, func(r *Run) (float64, bool) { v, ok := r.Loops.FirstPass.Value(); return v * 100, ok })
	num("attempts per loop", Lower, 2, func(r *Run) (float64, bool) { return r.Loops.AvgAttempts, r.Loops.Finished > 0 })
	num("reviewer veto (%)", Neutral, 2, func(r *Run) (float64, bool) { v, ok := r.Review.Vetoes.Value(); return v * 100, ok })
	num("interventions", Lower, 0, always(func(r *Run) float64 { return float64(r.Interventions.Total()) }))
	num("escalations", Lower, 0, always(func(r *Run) float64 { return float64(r.Interventions.Escalations) }))
	num("blocked tasks", Lower, 0, always(func(r *Run) float64 { return float64(len(r.BlockedTasks)) }))
	num("retries", Lower, 0, always(func(r *Run) float64 { return float64(r.Retries) }))
	graded := func(f func(*Grade) float64) func(*Run) (float64, bool) {
		return func(r *Run) (float64, bool) {
			if r.Grade == nil || r.Grade.Suite == "" {
				return 0, false
			}
			return f(r.Grade), true
		}
	}
	num("reward", Higher, 4, graded(func(g *Grade) float64 { return g.Reward }))
	num("fail-to-pass passed", Higher, 0, graded(func(g *Grade) float64 { return float64(g.FailToPass.Passed) }))
	num("escaped defects", Lower, 0, graded(func(g *Grade) float64 { return float64(g.EscapedDefects) }))
}

// Verdict reads the rows for criterion 7.
func (c *Comparison) Verdict() Verdict {
	v := Verdict{Improved: []string{}, Worse: []string{}}
	for _, r := range c.Rows {
		if !r.Valid || r.Better == Neutral || math.Abs(r.D) < 1e-9 {
			continue
		}
		if (r.D > 0) == (r.Better == Higher) {
			v.Improved = append(v.Improved, r.Name)
		} else {
			v.Worse = append(v.Worse, r.Name)
		}
		if r.Name == "reward" {
			v.RewardKnown = true
		}
	}
	for _, r := range c.Rows {
		if r.Name == "reward" && r.Valid {
			v.RewardKnown = true
			v.RewardHeld = r.D > -1e-9
		}
	}
	return v
}

// WriteText writes the comparison for a journal entry.
func (c *Comparison) WriteText(w io.Writer) {
	line(w, "compare:", "%s  to  %s", c.A, c.B)
	if c.Comparable {
		line(w, "comparable:", "yes (the same brief)")
	} else {
		line(w, "comparable:", "NO: %s", strings.Join(c.Reasons, "; "))
	}
	if len(c.Differences) == 0 {
		line(w, "what differed:", "nothing in the setup")
	} else {
		fmt.Fprintln(w, "what differed:")
		for _, d := range c.Differences {
			fmt.Fprintf(w, "  %-16s %s  ->  %s\n", d.What, d.A, d.B)
		}
	}
	fmt.Fprintf(w, "\n%-24s %12s %12s %10s\n", "metric", "A", "B", "delta")
	for _, r := range c.Rows {
		if !r.Valid {
			fmt.Fprintf(w, "%-24s %12s %12s %10s\n", r.Name, "n/a", "n/a", "")
			continue
		}
		fmt.Fprintf(w, "%-24s %12.*f %12.*f %+10.*f\n", r.Name, r.Dec, r.A, r.Dec, r.B, r.Dec, r.D)
	}
	v := c.Verdict()
	fmt.Fprintln(w)
	line(w, "improved:", "%s", orNone(strings.Join(v.Improved, ", ")))
	line(w, "worse:", "%s", orNone(strings.Join(v.Worse, ", ")))
	switch {
	case !v.RewardKnown && !v.hasReward(c):
		line(w, "reward:", "unknown (a run has no grade)")
	case v.RewardHeld:
		line(w, "reward:", "held (%.4f to %.4f)", c.a.Grade.Reward, c.b.Grade.Reward)
	default:
		line(w, "reward:", "NOT held (%.4f to %.4f)", c.a.Grade.Reward, c.b.Grade.Reward)
	}
}

func (v Verdict) hasReward(c *Comparison) bool {
	return c.a.Grade != nil && c.b.Grade != nil && c.a.Grade.Suite != "" && c.b.Grade.Suite != ""
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func setting(m map[string]Setting, name string) string {
	s, ok := m[name]
	if !ok {
		return "none"
	}
	return s.Agent + " " + s.Model
}

func union(a, b func(func(string) bool)) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	return slices.Sorted(maps.Keys(set))
}
