package engine

import (
	"os"
	"syscall"
	"testing"
)

// TestMain makes the test process a child subreaper that never reaps orphans, as a container's
// PID 1 may be (CI's is). Killed descendants then stay zombies, so a test that takes a zombie for
// a running process fails here as it would in CI.
func TestMain(m *testing.M) {
	const prSetChildSubreaper = 36
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		panic(e)
	}
	os.Exit(withStateFolder(m))
}

// withStateFolder points the operator's state folder, where the spend journal defaults to, at a
// folder of the test's own, so no test writes under the real HOME.
func withStateFolder(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ygg-state-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("XDG_STATE_HOME", dir)
	return m.Run()
}
