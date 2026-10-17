package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/budget"
)

// journalNodes returns the nodes of the entries in the day's journal, in order.
func journalNodes(t *testing.T, j *budget.Journal) []string {
	t.Helper()
	b, err := os.ReadFile(j.File(time.Now()))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var nodes []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if i := strings.Index(line, `"node":"`); i >= 0 {
			rest := line[i+len(`"node":"`):]
			nodes = append(nodes, rest[:strings.Index(rest, `"`)])
		}
	}
	return nodes
}

// Review I4: a spend the journal can't take is kept in run.json as pending, counted once in the run's
// usage, and written to the journal first, in order, when the journal takes lines again.
func TestASpendTheJournalMissedIsKeptPendingAndWrittenLater(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "spend")
	r := startToyRun(t, scriptedRoles+fullCaps+"  journal: "+journal+"\n")
	id, project := r.State.ID, r.Project
	// A file where the journal's folder should be: the journal can't take a line.
	if err := os.WriteFile(journal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ua, ub, uc := gungnir.Usage{InTok: 100, Turns: 3}, gungnir.Usage{InTok: 10, Turns: 1}, gungnir.Usage{InTok: 1, Turns: 1}
	if err := r.Spend("a", ua); err == nil {
		t.Fatal("Spend() = nil though the journal can't take the line")
	}
	s, err := LoadState(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Usage != ua || len(s.PendingSpend) != 1 || s.PendingSpend[0].Node != "a" || s.PendingSpend[0].Usage != ua || s.PendingSpend[0].Run != id {
		t.Fatalf("run.json usage = %+v, pending = %+v; want the spend counted once and pending", s.Usage, s.PendingSpend)
	}
	// The journal takes lines again: the next spend writes the pending one first.
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := r.Spend("b", ub); err != nil {
		t.Fatalf("Spend() = %v", err)
	}
	if got := journalNodes(t, r.Setup.Journal); strings.Join(got, ",") != "a,b" {
		t.Errorf("the day's journal holds %v, want a then b", got)
	}
	if s, _ = LoadState(r.Dir); len(s.PendingSpend) != 0 || r.State.Usage != ua.Add(ub) {
		t.Errorf("run.json pending = %+v, run usage = %+v; want none pending and each spend counted once", s.PendingSpend, r.State.Usage)
	}
	// A resume writes what is pending.
	if os.Getuid() == 0 {
		t.Skip("root writes to a file of mode 0400")
	}
	day := r.Setup.Journal.File(time.Now())
	if err := os.Chmod(day, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := r.Spend("c", uc); err == nil {
		t.Fatal("Spend() = nil though the journal can't take the line")
	}
	r.Close()
	if err := os.Chmod(day, 0o600); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer r2.Close()
	if got := journalNodes(t, r2.Setup.Journal); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("the day's journal holds %v after the resume, want a, b then c", got)
	}
	if s, _ = LoadState(r2.Dir); len(s.PendingSpend) != 0 || s.Usage != ua.Add(ub).Add(uc) {
		t.Errorf("run.json pending = %+v, usage = %+v; want none pending and each spend counted once", s.PendingSpend, s.Usage)
	}
}
