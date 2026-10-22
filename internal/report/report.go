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
		fmt.Fprintf(&b, "- %s: %s\n", inline(t.Task), inline(t.Why))
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
			fmt.Fprintf(&b, "- %s: %s. %s\n", inline(id), inline(e.Reason), inline(e.Detail))
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
			fmt.Fprintf(&b, "### %s in %s\n\n%s, from %s, at %s. Settled by the check %s.\n\nReviewer's claim:\n\n%s\n\nImplementer's evidence:\n\n%s\n\n",
				inline(o.ID), inline(id), inline(string(o.Severity)), inline(o.Source), inline(o.Location), code(o.OverruledBy), quote(o.Claim), quote(o.Proof))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// quote puts text an agent wrote inside a block quote, line by line, so it cannot open a heading, a table or
// a fence of the report. Every line break CommonMark knows ends a line, and each line is cleaned and escaped.
func quote(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n").Replace(s)
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace("> " + inline(l))
	}
	return strings.Join(lines, "\n")
}

// cell makes text fit in one table cell: inline escapes the pipe too.
func cell(s string) string { return inline(s) }

// oneLine makes text one line with no control or format character.
func oneLine(s string) string { return metrics.Clean(s) }

// inline makes text an agent wrote safe in the middle of a line: clean, then every character that can start
// Markdown syntax (a link, an image, HTML, emphasis, an entity, a table edge) is escaped. A character that only
// means something at the start of a line (a list marker, a setext underline) is escaped there.
func inline(s string) string {
	s = metrics.Clean(s)
	var b strings.Builder
	digits := true
	for i, r := range s {
		switch {
		case strings.ContainsRune("\\`*_{}[]<>()#!|~&", r):
			b.WriteByte('\\')
		case i == 0 && strings.ContainsRune("+-=", r):
			b.WriteByte('\\')
		case r == '.' && i > 0 && digits:
			b.WriteByte('\\')
		}
		if r < '0' || r > '9' {
			digits = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// code puts text in a code span whose fence is one backtick longer than any run of them in the text.
func code(s string) string {
	s = metrics.Clean(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}
