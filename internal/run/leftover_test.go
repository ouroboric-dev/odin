package run

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// leftover starts script in its own process group, as the engine starts exec nodes, and records
// the group in the run's run.json as an exec node a killed ygg left behind. It returns the pid
// that script wrote to $PWD/pid.
func leftover(t *testing.T, project, id, script string) (pgid, child int) {
	t.Helper()
	work := t.TempDir()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = work
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap the leader, as init would once ygg is gone
	pgid = cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL) })
	for deadline := time.Now().Add(5 * time.Second); child == 0; time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(filepath.Join(work, "pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if time.Now().After(deadline) {
			t.Fatal("the script never wrote its pid")
		}
	}
	dir := Dir(project, id)
	s, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Status, s.Exec = StatusRunning, &ExecProcess{Node: "dor", PGID: pgid, Start: procStart(pgid)}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	return pgid, child
}

func startToy(t *testing.T) (project, id string) {
	t.Helper()
	project = treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	return project, r.State.ID
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

func TestOpenStopsAnExecProcessAKilledYggLeftBehind(t *testing.T) {
	project, id := startToy(t)
	pgid, child := leftover(t, project, id, "sleep 30 & echo $! > pid; wait")
	r, err := Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer r.Close()
	for _, pid := range []int{pgid, child} {
		if alive(pid) {
			t.Errorf("process %d of the leftover group is still running", pid)
		}
	}
	if r.State.Exec != nil {
		t.Errorf("run.State.Exec = %+v after the group was stopped", r.State.Exec)
	}
}

func TestOpenRefusesWhenItCantTellALeftoverGroupIsTheRuns(t *testing.T) {
	project, id := startToy(t)
	// The leader exits at once, so only its background child keeps the group alive.
	_, child := leftover(t, project, id, "sleep 30 & echo $! > pid")
	if _, err := Open(ctx, project, id, ""); err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Errorf("Open() = %v, want a refusal that names the running group", err)
	}
	if !alive(child) {
		t.Error("Open() killed a group it couldn't tell was the run's")
	}
}

func TestDoctorsExportsFindAndStopALeftoverGroup(t *testing.T) {
	project, id := startToy(t)
	pgid, child := leftover(t, project, id, "sleep 30 & echo $! > pid; wait")
	s, err := LoadState(Dir(project, id))
	if err != nil {
		t.Fatal(err)
	}
	if !LeftoverProcess(s.Exec) {
		t.Fatal("LeftoverProcess() = false for a group that is running")
	}
	if err := StopLeftoverProcess(s.Exec); err != nil {
		t.Fatal(err)
	}
	if LeftoverProcess(s.Exec) || alive(pgid) || alive(child) {
		t.Error("the group is still running after StopLeftoverProcess")
	}
}
