package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func TestDoctorReportsMissingCaps(t *testing.T) {
	dir, config := project(t, "script.yaml", "budget:\n  per_run: { tokens: 100000, turns: 100 }\n")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR caps:") || !strings.Contains(out, "budget.per_day") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
}

func TestDoctorFindsAChangedLedger(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
	id := runID(t, dir)
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitDone || !strings.Contains(out, "OK    run "+id+": stopped") {
		t.Fatalf("doctor on a good run: exit %d:\n%s", code, out)
	}
	path := filepath.Join(dir, ".factory", "runs", id, "ledger.jsonl")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(b, []byte(`"gen_ai.usage.input_tokens":1000`), []byte(`"gen_ai.usage.input_tokens":1`), 1), 0o644)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR run "+id+": ledger:") {
		t.Errorf("doctor on a changed ledger: exit %d:\n%s", code, out)
	}
}

func TestDoctorChecksTheInstanceTree(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || !strings.Contains(out, "OK    tree: ") || !strings.Contains(out, "nodes") {
		t.Errorf("doctor in a project: exit %d:\n%s", code, out)
	}
	code, out, _ = ygg(t, "doctor", "--project", t.TempDir(), "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR tree: ") {
		t.Errorf("doctor outside a tree: exit %d:\n%s", code, out)
	}
}

func TestDoctorWarnsWhenAPublicUmbrellaListsAPrivateChild(t *testing.T) {
	root := treetest.Build(t, treetest.Node{Owner: "brunogbv", Visibility: gungnir.VisibilityPublic, Children: map[string]treetest.Node{
		"projects/secret": {Owner: "brunogbv", Visibility: gungnir.VisibilityPrivate}}})
	code, out, _ := ygg(t, "doctor", "--root", root)
	if code != ExitDone || !strings.Contains(out, "WARN  tree: ") || !strings.Contains(out, "projects/secret") {
		t.Errorf("doctor --root: exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "config") {
		t.Errorf("doctor --root alone checked a config:\n%s", out)
	}
}

func TestDoctorChecksTheSpendJournal(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || !strings.Contains(out, "OK    budget: this run 0 of 100000 tokens, 0 of 100 turns; today 0 of 200000 tokens, 0 of 200 turns (journal ") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
	// A journal that can't be read is an error: the day's cap can't be checked.
	journal := filepath.Join(t.TempDir(), "spend")
	os.MkdirAll(journal, 0o700)
	os.WriteFile(filepath.Join(journal, time.Now().Format("2006-01-02")+".jsonl"), []byte("not json\nnot json\n"), 0o600)
	b, _ := os.ReadFile(config)
	os.WriteFile(config, append(b, []byte("  journal: "+journal+"\n")...), 0o644)
	code, out, _ = ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR budget: ") || !strings.Contains(out, ".jsonl:1 is not a spend entry") {
		t.Errorf("doctor with a broken journal: exit %d:\n%s", code, out)
	}
	// A journal folder that can't be written to is an error too.
	if os.Getuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		os.MkdirAll(locked, 0o500)
		b, _ = os.ReadFile(config)
		os.WriteFile(config, []byte(strings.Replace(string(b), "  journal: "+journal, "  journal: "+filepath.Join(locked, "spend"), 1)), 0o644)
		if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitFailed || !strings.Contains(out, "ERROR budget: the spend journal can't be made") {
			t.Errorf("doctor with a folder it can't write: exit %d:\n%s", code, out)
		}
	}
}

// Review I4: the journal folder can be fine while today's file can't take a line; doctor opens today's
// file for appending, as a session's spend does.
func TestDoctorOpensTodaysJournalFileForAppending(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a file of mode 0400")
	}
	dir, config := project(t, "script.yaml", "")
	journal := filepath.Join(t.TempDir(), "spend")
	os.MkdirAll(journal, 0o700)
	today := filepath.Join(journal, time.Now().Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(today, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(config)
	os.WriteFile(config, append(b, []byte("  journal: "+journal+"\n")...), 0o644)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR budget: ") || !strings.Contains(out, today) {
		t.Errorf("doctor with today's journal file read-only: exit %d:\n%s", code, out)
	}
	if err := os.Chmod(today, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitDone || !strings.Contains(out, "OK    budget: ") {
		t.Errorf("doctor with today's journal file writable: exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(today); len(b) != 0 {
		t.Errorf("doctor wrote %q to today's journal file; it must write nothing", b)
	}
}
