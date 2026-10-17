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

// Cleanup review: a spend is written only after every pending one, even when a save failed after a
// pending one was written, so the journal keeps the order.
func TestASpendWaitsBehindThePendingOnesWhenASaveFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a folder of mode 0500")
	}
	journal := filepath.Join(t.TempDir(), "spend")
	r := startToyRun(t, scriptedRoles+fullCaps+"  journal: "+journal+"\n")
	defer r.Close()
	if err := os.WriteFile(journal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	u := gungnir.Usage{InTok: 1, Turns: 1}
	r.Spend("p1", u)
	r.Spend("p2", u)
	if len(r.State.PendingSpend) != 2 {
		t.Fatalf("pending = %+v, want p1 and p2", r.State.PendingSpend)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	// run.json can't be saved: p1 is written, and the save after it fails.
	if err := os.Chmod(r.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := r.Spend("n", u); err == nil {
		t.Error("Spend() = nil though run.json couldn't be saved")
	}
	if err := os.Chmod(r.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.Spend("m", u); err != nil {
		t.Fatalf("Spend() = %v", err)
	}
	if got := journalNodes(t, r.Setup.Journal); strings.Join(got, ",") != "p1,p2,n,m" {
		t.Errorf("the day's journal holds %v, want p1, p2, n then m", got)
	}
}

// Cleanup review: a pending entry the journal already holds, because the process stopped between its
// append and the save that dropped it, is not written again.
func TestAPendingEntryTheJournalHoldsIsNotWrittenTwice(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "spend")
	r := startToyRun(t, scriptedRoles+fullCaps+"  journal: "+journal+"\n")
	defer r.Close()
	e := r.Setup.Journal.Stamp(budget.Entry{Run: r.State.ID, Project: r.Project, Node: "p", Usage: gungnir.Usage{InTok: 1, Turns: 1}})
	if err := r.Setup.Journal.Append(e); err != nil {
		t.Fatal(err)
	}
	r.State.PendingSpend = []budget.Entry{e} // appended, and the process stopped before the save
	if err := r.Spend("q", gungnir.Usage{InTok: 2, Turns: 1}); err != nil {
		t.Fatalf("Spend() = %v", err)
	}
	if got := journalNodes(t, r.Setup.Journal); strings.Join(got, ",") != "p,q" {
		t.Errorf("the day's journal holds %v, want p once, then q", got)
	}
	if len(r.State.PendingSpend) != 0 {
		t.Errorf("pending = %+v, want none", r.State.PendingSpend)
	}
}

// Cleanup review: a resume refused because a pending spend still can't be written changes nothing in
// the leaves.
func TestAResumeRefusedForAPendingSpendLeavesTheLeavesAlone(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "spend")
	r := startToyRun(t, scriptedRoles+fullCaps+"  journal: "+journal+"\n")
	id, project, app := r.State.ID, r.Project, r.RepoDir("repos/app")
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Spend("a", gungnir.Usage{InTok: 1, Turns: 1}); err == nil {
		t.Fatal("Spend() = nil though the journal can't take the line")
	}
	r.Close()
	if err := os.WriteFile(filepath.Join(app, "half-done.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r2, err := Open(ctx, project, id, ""); err == nil {
		r2.Close()
		t.Fatal("Open() = nil though the pending spend can't be written")
	}
	if _, err := os.Stat(filepath.Join(app, "half-done.txt")); err != nil {
		t.Errorf("the refused resume reset the leaf: %v", err)
	}
	if refs, _ := l.Git(ctx, "for-each-ref", "refs/ygg/"+id+"/discarded/"); refs != "" {
		t.Errorf("the refused resume saved discarded work: %s", refs)
	}
}
