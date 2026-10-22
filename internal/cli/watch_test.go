package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
)

// A run that a process holds is live. The test holds the run's lock itself, as ygg does.
func TestStatusWatchPrintsAgainEveryIntervalUntilTheRunStopsRunning(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	runDir := run.Dir(dir, id)
	st, err := run.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Status = run.StatusRunning
	if err := st.Save(runDir); err != nil {
		t.Fatal(err)
	}
	release, err := lock.Take(filepath.Join(runDir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	go func() {
		time.Sleep(400 * time.Millisecond)
		st.Status = run.StatusDone
		st.Save(runDir) //nolint:errcheck // the test reads what the watch printed
	}()
	code, out, errOut := ygg(t, "status", id, "--watch", "--interval", "100ms", "--project", dir)
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if n := strings.Count(out, "run:      "+id); n < 3 {
		t.Errorf("the watch printed the status %d times, want at least 3 before the run ended:\n%s", n, out)
	}
	if !strings.Contains(out[strings.LastIndex(out, "run:      "+id):], "status:   done") {
		t.Errorf("the last block does not show the run as done:\n%s", out)
	}
}

func TestStatusWatchOfARunNoProcessHoldsPrintsOnceAndReturns(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	runDir := run.Dir(dir, id)
	st, _ := run.LoadState(runDir)
	st.Status = run.StatusRunning // the process died: nothing holds the lock
	st.Save(runDir)               //nolint:errcheck // checked by the status below
	done := make(chan struct{})
	var out string
	go func() {
		_, out, _ = ygg(t, "status", id, "--watch", "--interval", "50ms", "--project", dir)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch of an interrupted run never returned")
	}
	if n := strings.Count(out, "run:      "+id); n != 1 || !strings.Contains(out, "interrupted") {
		t.Errorf("printed %d times:\n%s", n, out)
	}
}

func TestStatusWatchNeedsAPositiveIntervalAndOnlyWithWatch(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	for name, args := range map[string][]string{
		"a zero interval":      {"status", id, "--watch", "--interval", "0s", "--project", dir},
		"an interval too long": {"status", id, "--watch", "--interval", "2h", "--project", dir},
		"an interval alone":    {"status", id, "--interval", "5s", "--project", dir},
	} {
		if code, _, errOut := ygg(t, args...); code != ExitRefused || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q; want a refusal", name, code, errOut)
		}
	}
}
