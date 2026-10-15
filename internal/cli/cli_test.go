package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ygg runs one command in-process and returns its exit code, stdout and stderr.
func ygg(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// project makes a project directory with a factory.yaml for the toy realm. Its roles use script,
// a file in testdata/toy; extra is added to the config as is.
func project(t *testing.T, script, extra string) (dir, config string) {
	t.Helper()
	toy, err := filepath.Abs("../../testdata/toy")
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	config = filepath.Join(dir, "factory.yaml")
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
	if code != ExitDone || !strings.Contains(out, "waiting:  design-gate") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	// Flags may follow the positional arguments.
	if code, out, errOut := ygg(t, "approve", id, "design-gate", "--project", dir, "--note", "fine"); code != ExitDone {
		t.Errorf("approve: exit %d:\n%s%s", code, out, errOut)
	}
	if code, _, errOut := ygg(t, "resume", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "is done") {
		t.Errorf("resume of a done run: exit %d, %q", code, errOut)
	}
}
