// Package report renders the run report of spec §10.3 as Markdown: the tasks the run gave up first (spec §7.3),
// the metrics, the interventions, and the findings a dispute overruled with both sides (B-115).
package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/engine"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// Write renders the report. The same run always gives the same bytes: nothing in it is the time of writing.
func Write(w io.Writer, m *metrics.Run, st *run.State) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run %s\n\n", m.ID)
	fmt.Fprintf(&b, "%s, %s mode, graph %s %s. Read from ledger line %d (sha256 %.12s): quote the run id and that line to cite a number.\n\n",
		m.Status, m.Mode, m.Graph, m.GraphVersion, st.Ledger.Seq, st.Ledger.Hash)

	b.WriteString("## Blocked tasks\n\n")
	if len(m.BlockedTasks) == 0 {
		b.WriteString("None.\n\n")
	}
	for _, t := range m.BlockedTasks {
		fmt.Fprintf(&b, "- %s: %s\n", t.Task, oneLine(t.Why))
	}
	if len(m.BlockedTasks) > 0 {
		b.WriteString("\n")
	}

	var text strings.Builder
	m.WriteText(&text)
	// The block is a fence, and part of its text can come from an agent: no line of it may be a fence.
	b.WriteString("## Metrics\n\n```text\n" + strings.ReplaceAll(text.String(), "```", "'''") + "```\n\n")

	b.WriteString("## Interventions\n\n")
	if len(m.Interventions.Items) == 0 {
		b.WriteString("None.\n\n")
	} else {
		b.WriteString("| Kind | Where | Note |\n| --- | --- | --- |\n")
		for _, it := range m.Interventions.Items {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(it.Kind), cell(it.Node), cell(it.Note))
		}
		b.WriteString("\n")
	}

	gave := false
	for _, id := range slices.Sorted(maps.Keys(st.Loops)) {
		if e := st.Loops[id].Escalation; e != nil {
			if !gave {
				b.WriteString("## Loops that gave up\n\n")
				gave = true
			}
			fmt.Fprintf(&b, "- %s: %s. %s\n", id, oneLine(e.Reason), oneLine(e.Detail))
		}
	}
	if gave {
		b.WriteString("\n")
	}

	overruled := false
	for _, id := range slices.Sorted(maps.Keys(st.Loops)) {
		for _, o := range slices.SortedFunc(slices.Values(engine.Overruled(&st.Loops[id].State)), func(a, b jormungandr.Record) int { return cmp.Compare(a.ID, b.ID) }) {
			if !overruled {
				b.WriteString("## Findings a dispute overruled\n\nEach is the implementer's word, backed by one check. Read both sides.\n\n")
				overruled = true
			}
			fmt.Fprintf(&b, "### %s in %s\n\n%s, from %s, at %s. Settled by the check `%s`.\n\nReviewer's claim:\n\n%s\n\nImplementer's evidence:\n\n%s\n\n",
				o.ID, id, o.Severity, o.Source, oneLine(o.Location), oneLine(o.OverruledBy), quote(o.Claim), quote(o.Proof))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// quote puts text an agent wrote inside a block quote, line by line, so it cannot open a heading, a table or
// a fence of the report.
func quote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// cell makes text fit in one table cell.
func cell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
