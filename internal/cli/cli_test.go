package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// ygg runs one command in-process and returns its exit code, stdout and stderr.
func ygg(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// project makes a project umbrella in a fresh instance tree, and a factory.yaml for the toy realm
// beside it. Its roles use script, a file in testdata/toy; extra is added to the config as is.
func project(t *testing.T, script, extra string) (dir, config string) {
	t.Helper()
	// Each test has a spend journal of its own, so what one spends never counts against another's day.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	toy, err := filepath.Abs("../../testdata/toy")
	if err != nil {
		t.Fatal(err)
	}
	dir = treetest.Project(t) // the toy realm creates its leaf, repos/app
	config = filepath.Join(t.TempDir(), "factory.yaml")
	s := filepath.Join(toy, script)
	body := extra + "roles:\n  planner: { agent: scripted, model: none, script: " + s + " }\n" +
		"  implementer: { agent: scripted, model: none, script: " + s + " }\n"
	if !strings.Contains(extra, "budget:") {
		body += "budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	}
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, config
}

func toyRealm(t *testing.T) string {
	t.Helper()
	p, _ := filepath.Abs("../../testdata/toy")
	return p
}

// runID returns the id of the only run in dir.
func runID(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one run, got %v, %v", entries, err)
	}
	return entries[0].Name()
}

func TestUsageErrorsRefuseAndCreateNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	cases := map[string][]string{
		"no command":            {},
		"an unknown command":    {"deploy"},
		"run without --realm":   {"run", "--project", dir, "--config", config},
		"run with a bad mode":   {"run", "--project", dir, "--config", config, "--realm", toyRealm(t), "--mode", "fast"},
		"approve without gate":  {"approve", "20261015-090000-a1b2c3", "--project", dir},
		"a path as the run id":  {"approve", "../../../tmp", "design-gate", "--project", dir},
		"status of a path":      {"status", "../..", "--project", dir},
		"resume of no such run": {"resume", "20261015-090000-a1b2c3", "--project", dir},
	}
	for name, args := range cases {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, code, ExitRefused)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory")); err == nil {
		t.Error("a refused command created .factory/")
	}
}

func TestRunStopsAndApproveContinues(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point design-gate") {
		t.Fatalf("run: exit %d, %q; want 3 at design-gate", code, out)
	}
	id := runID(t, dir)
	code, out, _ = ygg(t, "status", id, "--project", dir)
	if code != ExitDone || !strings.Contains(out, "waiting:  design-gate (design)") || !strings.Contains(out, "leaf:     repos/app on ygg/"+id+" at ") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	// Each decision takes only its own flag.
	if code, _, _ := ygg(t, "approve", id, "design-gate", "--project", dir, "--reason", "x"); code != ExitRefused {
		t.Errorf("approve --reason: exit %d, want %d", code, ExitRefused)
	}
	if code, _, _ := ygg(t, "reject", id, "design-gate", "--project", dir, "--note", "x"); code != ExitRefused {
		t.Errorf("reject --note: exit %d, want %d", code, ExitRefused)
	}
	// Flags may follow the positional arguments.
	if code, out, errOut := ygg(t, "approve", id, "design-gate", "--project", dir, "--note", "fine"); code != ExitDone {
		t.Errorf("approve: exit %d:\n%s%s", code, out, errOut)
	}
	if code, _, errOut := ygg(t, "resume", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "is done") {
		t.Errorf("resume of a done run: exit %d, %q", code, errOut)
	}
}

// A refused command exits 2 and changes nothing, even when it asks for another mode and the config
// changed since the last command (decision 10).
func TestARefusedCommandChangesNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
	id := runID(t, dir)
	runDir := filepath.Join(dir, ".factory", "runs", id)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(runDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cfg, _ := os.ReadFile(config)
	os.WriteFile(config, append(cfg, "privacy: strict\n"...), 0o644)
	state, ledger := read("run.json"), read("ledger.jsonl")
	for _, args := range [][]string{
		{"approve", id, "design-gat", "--mode", "unattended"},
		{"reject", id, "design-gate", "--reason", " ", "--mode", "unattended"},
	} {
		if code, _, _ := ygg(t, append(args, "--project", dir)...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", args[0], code, ExitRefused)
		}
		if read("run.json") != state || read("ledger.jsonl") != ledger {
			t.Errorf("a refused %s changed run.json or the ledger", args[0])
		}
	}
	if code, _, _ := ygg(t, "resume", id, "--project", dir, "--mode", "unattended"); code != ExitStopped {
		t.Fatalf("resume of a stopped run: exit %d, want %d", code, ExitStopped)
	}
	if l := read("ledger.jsonl"); !strings.Contains(l, `"event":"config_changed"`) || !strings.Contains(l, `"event":"mode_changed"`) {
		t.Errorf("resume at a pause didn't record the switch:\n%s", l)
	}
	ygg(t, "approve", id, "design-gate", "--project", dir)
	state, ledger = read("run.json"), read("ledger.jsonl")
	if code, _, _ := ygg(t, "resume", id, "--project", dir, "--mode", "step-by-step"); code != ExitRefused {
		t.Errorf("resume of a done run: exit %d, want %d", code, ExitRefused)
	}
	if read("run.json") != state || read("ledger.jsonl") != ledger {
		t.Error("a refused resume of a done run changed run.json or the ledger")
	}
}

// A run that fails after it started is recorded as failed and exits 1, with the resume hint
// (decision 16); a refused start exits 2.
func TestAFailedStartExitsOne(t *testing.T) {
	_, config := project(t, "script.yaml", "")
	dir := treetest.Project(t, "repos/app")
	if _, err := tree.Resolve(context.Background(), dir, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos", "app", "README.md"), []byte("a person's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
	id := runID(t, dir)
	if code != ExitFailed || !strings.Contains(errOut, "ygg resume "+id) || !strings.Contains(errOut, "refs/ygg/"+id+"/discarded/") {
		t.Errorf("run with a dirty leaf: exit %d, want %d with the resume hint:\n%s", code, ExitFailed, errOut)
	}
}
