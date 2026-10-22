package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
)

const labelWidth = 14

// line writes "label value", the label padded to a column.
func line(w io.Writer, label, format string, args ...any) {
	pad := " "
	if n := labelWidth - len(label); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(w, "%s%s%s\n", label, pad, fmt.Sprintf(format, args...))
}

// WriteText writes the report the operator reads and pastes into a journal entry: every figure of spec §10.2,
// in words, with a rate that has nothing to count saying so.
func (r *Run) WriteText(w io.Writer) {
	line(w, "run:", "%s  (%s, %s, graph %s %s)", r.ID, r.Status, r.Mode, r.Graph, r.GraphVersion)
	if len(r.Factory) == 0 {
		line(w, "factory:", "not recorded (a run from before the ledger named its build)")
	} else {
		line(w, "factory:", "%s", strings.Join(r.Factory, "; "))
	}
	line(w, "config:", "%s  brief: %s%s", short(r.ConfigSHA256), short(r.BriefSHA256), realms(r.Realms))
	label := "roles:"
	for _, name := range slices.Sorted(maps.Keys(r.Roles)) {
		line(w, label, "%s = %s %s", name, r.Roles[name].Agent, r.Roles[name].Model)
		label = ""
	}
	line(w, "time:", "%s active of %s elapsed (approval waits %s, stopped %s)", dur(r.Time.Active), dur(r.Time.Elapsed), dur(r.Time.ApprovalWait), dur(r.Time.Stopped))
	u := r.Usage
	line(w, "usage:", "%s tokens in, %s out, %s from cache; %d turns; %d sessions", num(u.InTok), num(u.OutTok), num(u.CacheTok), u.Turns, u.Sessions)
	cost := fmt.Sprintf("$%.4f at API prices, $%.4f of it billed to a paid API", u.CostUSD, u.BilledUSD)
	if u.Unpriced > 0 {
		cost += fmt.Sprintf("; %d sessions had tokens and no cost, so this is a floor", u.Unpriced)
	}
	line(w, "cost:", "%s", cost)
	l := r.Loops
	line(w, "loops:", "%d ended: first pass %s, %.2f attempts on average, %d gave up", l.Finished, ratio(l.FirstPass, "", "no loop has ended"), l.AvgAttempts, l.GaveUp)
	line(w, "review:", "reviewer vetoes %s", ratio(r.Review.Vetoes, "attempts", "no reviewer has run"))
	f := r.Findings
	raised := f.RaisedByCheck + f.RaisedByReview + f.RaisedUnattributed
	by := fmt.Sprintf("%d by checks, %d by reviewers", f.RaisedByCheck, f.RaisedByReview)
	if f.RaisedUnattributed > 0 {
		by += fmt.Sprintf(", %d with no origin recorded", f.RaisedUnattributed)
	}
	line(w, "findings:", "raised %d (%s); closed %d; reopened %d; overruled %d; disputes ignored %d", raised, by, f.Closed, f.Reopened, f.Overruled, f.DisputesIgnored)
	i := r.Interventions
	line(w, "interventions:", "%d (%d rejections, %d approvals with notes, %d escalations)", i.Total(), i.Rejections, i.NotedApprovals, i.Escalations)
	if len(r.BlockedTasks) == 0 {
		line(w, "blocked:", "none")
	}
	for n, b := range r.BlockedTasks {
		label := ""
		if n == 0 {
			label = "blocked:"
		}
		line(w, label, "%s: %s", clean(b.Task), clean(b.Why))
	}
	r.writeGrade(w)
	rt := r.Routing
	bench := "bench not measured"
	if rt.Bench != nil {
		bench = fmt.Sprintf("bench %.0f%% (grep baseline %.0f%%, bar %.0f%%)", *rt.Bench*100, *rt.Baseline*100, *rt.Threshold*100)
	}
	line(w, "routing:", "%d lookups, %d indexes built; %s", rt.Lookups, rt.Indexes, bench)
	if r.Retries > 0 {
		line(w, "retries:", "%d sessions were retried after a transient failure", r.Retries)
	}
	for _, msg := range r.Warnings {
		line(w, "warning:", "%s", clean(msg))
	}
	if len(r.ByRole) > 0 {
		fmt.Fprintln(w, "\nby role:")
		for _, k := range slices.Sorted(maps.Keys(r.ByRole)) {
			fmt.Fprintf(w, "  %-45s %s\n", k, usageLine(r.ByRole[k]))
		}
	}
	if len(r.Tasks) > 0 {
		fmt.Fprintln(w, "\ntasks:")
		for _, t := range r.Tasks {
			fmt.Fprintf(w, "  %-16s %-8s %-12s %s\n", t.ID, t.Status, t.Leaf, usageLine(t.Usage))
		}
	}
}

func (r *Run) writeGrade(w io.Writer) {
	g := r.Grade
	switch {
	case g == nil:
		line(w, "grade:", "not graded")
	case g.Refused != "" && g.Suite == "":
		line(w, "grade:", "refused: %s", clean(g.Refused))
	default:
		line(w, "grade:", "reward %.4f (suite %s of %s): fail-to-pass %d/%d, pass-to-pass %d/%d; escaped defects %d%s",
			g.Reward, g.Suite, g.Task, g.FailToPass.Passed, g.FailToPass.Total, g.PassToPass.Passed, g.PassToPass.Total, g.EscapedDefects, given(g))
		for _, name := range slices.Sorted(maps.Keys(g.Categories)) {
			c := g.Categories[name]
			line(w, "", "%s %d/%d (weight %.2f)", name, c.Passed, c.Total, c.Weight)
		}
	}
}

func given(g *Grade) string {
	if g.TasksGivenUp == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d tasks were given up: some failures are missing work)", g.TasksGivenUp)
}

func usageLine(u Usage) string {
	if u.Sessions == 0 && u.Turns == 0 {
		return ""
	}
	return fmt.Sprintf("%d turns, %d sessions, $%.4f", u.Turns, u.Sessions, u.CostUSD)
}

// WriteRange writes one row for each run and a total line. It adds no figure the rows do not show, such as a
// mean reward: a mean over runs of different briefs or suites would look like a measure.
func WriteRange(w io.Writer, runs []*Run) {
	fmt.Fprintf(w, "%-24s %-8s %-12s %9s %10s %12s %5s %7s  %s\n", "run", "status", "mode", "active", "cost", "first pass", "ints", "blocked", "reward")
	var active time.Duration
	var cost float64
	var ints, blocked int
	for _, r := range runs {
		reward := "not graded"
		if r.Grade != nil && r.Grade.Suite != "" {
			reward = fmt.Sprintf("%.4f", r.Grade.Reward)
		}
		first := "n/a"
		if v, ok := r.Loops.FirstPass.Value(); ok {
			first = fmt.Sprintf("%d/%d %.0f%%", r.Loops.FirstPass.N, r.Loops.FirstPass.D, v*100)
		}
		fmt.Fprintf(w, "%-24s %-8s %-12s %9s %10s %12s %5d %7d  %s\n", r.ID, r.Status, r.Mode, dur(r.Time.Active), fmt.Sprintf("$%.4f", r.Usage.CostUSD), first, r.Interventions.Total(), len(r.BlockedTasks), reward)
		active += time.Duration(r.Time.Active)
		cost += r.Usage.CostUSD
		ints += r.Interventions.Total()
		blocked += len(r.BlockedTasks)
	}
	fmt.Fprintf(w, "%-24s %-8s %-12s %9s %10s %12s %5d %7d  %s\n", "total", fmt.Sprintf("%d runs", len(runs)), "", dur(Duration(active)), fmt.Sprintf("$%.4f", cost), "", ints, blocked, "")
}

func realms(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+" "+m[k])
	}
	return "  realms: " + strings.Join(parts, ", ")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "none"
	}
	return sha
}

func ratio(r Ratio, unit, empty string) string {
	v, ok := r.Value()
	if !ok {
		return "n/a (" + empty + ")"
	}
	if unit != "" {
		unit += " "
	}
	return fmt.Sprintf("%d of %d %s(%.0f%%)", r.N, r.D, unit, v*100)
}

// dur writes a duration as 1h05m, 5m05s or 45s.
func dur(d Duration) string {
	t := time.Duration(d).Round(time.Second)
	h, m, s := int(t.Hours()), int(t.Minutes())%60, int(t.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0 && s == 0:
		return fmt.Sprintf("%dm", m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// num writes an integer with thousands separators.
func num(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// clean makes text an agent may have written safe to print: one line, and no control or format character, which a
// terminal would act on.
func clean(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)), " ")
}
