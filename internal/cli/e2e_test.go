package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
)

// TestMain lets a test run this binary as ygg: YGG_E2E_MAIN=1 <test binary> <ygg args>.
func TestMain(m *testing.M) {
	if os.Getenv("YGG_E2E_MAIN") == "1" {
		os.Exit(Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// walk starts the toy run and approves every stop. It returns the gates it stopped at, in order.
func walk(t *testing.T, dir, config string, runArgs ...string) (string, []string) {
	t.Helper()
	args := append([]string{"run", "--project", dir, "--config", config, "--realm", toyRealm(t)}, runArgs...)
	code, out, errOut := ygg(t, args...)
	id := runID(t, dir)
	var stops []string
	for code == ExitStopped {
		s, err := run.LoadState(run.Dir(dir, id))
		if err != nil {
			t.Fatal(err)
		}
		stops = append(stops, s.Pending)
		if len(stops) > 10 {
			t.Fatalf("more than 10 stops: %v", stops)
		}
		code, out, errOut = ygg(t, "approve", id, s.Pending, "--project", dir)
	}
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	return id, stops
}

// events returns the event names in the run's ledger, after checking its chain against run.json.
func events(t *testing.T, dir, id string) []string {
	t.Helper()
	s, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run.Dir(dir, id), "ledger.jsonl")
	if _, err := ledger.Verify(path, s.Ledger); err != nil {
		t.Fatalf("the ledger doesn't verify: %v", err)
	}
	b, _ := os.ReadFile(path)
	var names []string
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var e struct{ Event string }
		json.Unmarshal(line, &e)
		names = append(names, e.Event)
	}
	return names
}

func count(names []string, name string) int {
	n := 0
	for _, x := range names {
		if x == name {
			n++
		}
	}
	return n
}

func TestToyGraphEndToEnd(t *testing.T) {
	modes := []struct {
		name, extra, mode string
		stops             []string
	}{
		{"step-by-step stops at every approval point", "", "step-by-step",
			[]string{"design-gate", "spec-gate", "plan-gate", "feature-gate"}},
		{"key-points stops only at design", "", "key-points", []string{"design-gate"}},
		{"unattended stops at design by default", "", "unattended", []string{"design-gate"}},
		{"unattended with design: auto never stops", "gates: { design: auto }\n", "unattended", nil},
		{"a plan: stop override adds a stop in key-points", "gates: { plan: stop }\n", "key-points",
			[]string{"design-gate", "plan-gate"}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			dir, config := project(t, "script.yaml", "mode: "+m.mode+"\n"+m.extra)
			id, stops := walk(t, dir, config)
			if !slices.Equal(stops, m.stops) {
				t.Errorf("stops = %v, want %v", stops, m.stops)
			}
			names := events(t, dir, id)
			if got := count(names, "gate_pending"); got != len(m.stops) {
				t.Errorf("gate_pending = %d, want %d", got, len(m.stops))
			}
			if got := count(names, "gate_auto_passed"); got != 4-len(m.stops) {
				t.Errorf("gate_auto_passed = %d, want %d", got, 4-len(m.stops))
			}
			if got := count(names, "gate_approved"); got != len(m.stops) {
				t.Errorf("gate_approved = %d, want %d", got, len(m.stops))
			}
			gates, _ := os.ReadDir(filepath.Join(run.Dir(dir, id), "gates"))
			if len(gates) != 4 {
				t.Errorf("%d gate files, want 4 (one per approval point, stopped or not)", len(gates))
			}
			if names[len(names)-1] != "run_finished" {
				t.Errorf("the last event is %s, want run_finished", names[len(names)-1])
			}
			if b, err := os.ReadFile(filepath.Join(dir, "workspace", "build.txt")); err != nil || string(b) != "done\n" {
				t.Errorf("workspace/build.txt = %q, %v", b, err)
			}
		})
	}

	t.Run("a rejection re-runs the node with the reason", func(t *testing.T) {
		dir, config := project(t, "script.yaml", "mode: key-points\ngates: { spec: stop }\n")
		ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
		id := runID(t, dir)
		ygg(t, "approve", id, "design-gate", "--project", dir)
		code, _, errOut := ygg(t, "reject", id, "spec-gate", "--project", dir, "--reason", "Name the users.")
		if code != ExitStopped {
			t.Fatalf("reject: exit %d, %s; want 3 at spec-gate again", code, errOut)
		}
		b, _ := os.ReadFile(filepath.Join(run.Dir(dir, id), "transcripts", "spec-2.json"))
		if !strings.Contains(string(b), "Name the users.") {
			t.Errorf("spec's second attempt didn't get the reason:\n%s", b)
		}
		if code, _, _ := ygg(t, "approve", id, "spec-gate", "--project", dir); code != ExitDone {
			t.Errorf("approve: exit %d, want 0", code)
		}
		if spec, _ := os.ReadFile(filepath.Join(dir, "workspace", "spec.md")); !strings.Contains(string(spec), "photographers") {
			t.Errorf("spec.md = %q, want attempt 2's text", spec)
		}
	})

	t.Run("a killed run resumes at the interrupted node", func(t *testing.T) {
		dir, config := project(t, "script-hang.yaml", "mode: unattended\ngates: { design: auto }\n")
		cmd := exec.Command(os.Args[0], "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
		cmd.Env = append(os.Environ(), "YGG_E2E_MAIN=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		var id string
		for {
			if entries, _ := os.ReadDir(filepath.Join(dir, ".factory", "runs")); len(entries) == 1 {
				id = entries[0].Name()
				b, _ := os.ReadFile(filepath.Join(run.Dir(dir, id), "ledger.jsonl"))
				if strings.Contains(string(b), `"ygg.node.id":"build","event":"node_started"`) {
					break
				}
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatal("build never started")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cmd.Process.Kill()
		cmd.Wait()
		code, out, _ := ygg(t, "status", id, "--project", dir)
		if code != ExitDone || !strings.Contains(out, "interrupted") {
			t.Errorf("status after the kill: exit %d:\n%s", code, out)
		}
		if code, out, errOut := ygg(t, "resume", id, "--project", dir); code != ExitDone {
			t.Fatalf("resume: exit %d:\n%s%s", code, out, errOut)
		}
		s, _ := run.LoadState(run.Dir(dir, id))
		if s.Attempts["build"] != 2 || s.Attempts["design"] != 1 {
			t.Errorf("attempts = %v; want build 2 (the killed one counts) and design 1 (not re-run)", s.Attempts)
		}
		if names := events(t, dir, id); count(names, "run_resumed") != 1 {
			t.Errorf("run_resumed = %d, want 1", count(names, "run_resumed"))
		}
	})

	t.Run("no complete caps, no run", func(t *testing.T) {
		dir, config := project(t, "script.yaml", "budget:\n  per_run: { tokens: 100000, turns: 100 }\n")
		code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
		if code != ExitRefused || !strings.Contains(errOut, "budget.per_day") || !strings.Contains(errOut, "per_day: { tokens: <tokens>") {
			t.Errorf("run: exit %d:\n%s", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(dir, ".factory")); err == nil {
			t.Error("a refused run created .factory/")
		}
	})

	t.Run("a stopped run refuses to continue once the caps are removed", func(t *testing.T) {
		dir, config := project(t, "script.yaml", "")
		ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
		id := runID(t, dir)
		b, _ := os.ReadFile(config)
		os.WriteFile(config, []byte(strings.Split(string(b), "budget:")[0]), 0o644)
		for _, args := range [][]string{{"resume", id}, {"approve", id, "design-gate"}} {
			code, _, errOut := ygg(t, append(args, "--project", dir)...)
			if code != ExitRefused || !strings.Contains(errOut, "budget.per_run") {
				t.Errorf("%s: exit %d:\n%s", args[0], code, errOut)
			}
		}
	})
}
