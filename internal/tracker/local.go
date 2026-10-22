// Package tracker is the local tracker of spec §10.4: it projects the factory's state one way, into a board.
// It reads .factory/features.json and the run that file names, and it writes nothing.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// Local is the tracker of a project folder.
type Local struct{ Project string }

var _ gungnir.Tracker = Local{}

// Board puts each task of the plan in a column. A project with no plan yet has an empty board, which is not an
// error. A run.json that cannot be read leaves the board to the statuses of features.json alone.
func (l Local) Board(ctx context.Context) (gungnir.Board, error) {
	b := gungnir.Board{Project: l.Project}
	f, err := schema.LoadFeatures(filepath.Join(l.Project, ".factory", "features.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	st, err := run.LoadState(run.Dir(l.Project, f.Run))
	if err != nil {
		st = nil
	}
	for _, t := range f.Tasks {
		col, why := place(t, st)
		b.Cards = append(b.Cards, gungnir.Card{Task: t.ID, Title: t.Title, Leaf: t.Layer, Column: col, Why: why, Run: f.Run})
	}
	return b, b.Validate()
}

// place decides the column of one task. A task that is doing is in progress while its implementer works,
// verifying once the checks and reviewers judge it or its loop has passed, and needs the person when the run
// stopped at one of its gates or stopped because something failed.
func place(t schema.FeatureRecord, st *run.State) (gungnir.Column, string) {
	switch t.Status {
	case schema.TaskDone:
		return gungnir.ColumnDone, ""
	case schema.TaskBlocked:
		return gungnir.ColumnBlocked, oneLine(t.Why)
	case schema.TaskTodo:
		return gungnir.ColumnReady, ""
	}
	if st == nil {
		return gungnir.ColumnInProgress, ""
	}
	mine := func(node string) bool { return strings.HasPrefix(node, t.Slug()+"-") }
	switch st.Status {
	case run.StatusStopped:
		if mine(st.Pending) {
			return gungnir.ColumnNeedsYou, fmt.Sprintf("waiting at %s (%s)", st.Pending, st.PendingGate)
		}
	case run.StatusFailed:
		return gungnir.ColumnNeedsYou, "run failed: " + oneLine(st.Error)
	case run.StatusPaused:
		if st.Paused != nil {
			return gungnir.ColumnNeedsYou, "run paused: " + oneLine(st.Paused.Reason)
		}
	}
	for id, l := range st.Loops {
		if mine(id) && (l.State.Passed || l.State.Phase == jormungandr.PhaseLadder) {
			return gungnir.ColumnVerifying, ""
		}
	}
	return gungnir.ColumnInProgress, ""
}

func oneLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.Join(strings.Fields(line), " ")
}

// WriteBoard writes a board as text: each column with its count and its cards in the plan's order.
func WriteBoard(w io.Writer, b gungnir.Board) {
	fmt.Fprintf(w, "%s (%s)\n", b.Project, b.Summary())
	for _, col := range gungnir.Columns() {
		n := b.Counts()[col]
		fmt.Fprintf(w, "\n%s (%d)\n", col.Title(), n)
		for _, c := range b.Cards {
			if c.Column != col {
				continue
			}
			line := fmt.Sprintf("  %-16s %-12s %s", c.Task, c.Leaf, c.Title)
			if c.Why != "" {
				line += "  [" + clip(c.Why, maxWhy) + "]"
			}
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}
}

// maxWhy is how much of a reason the board shows; the card keeps all of it.
const maxWhy = 120

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
