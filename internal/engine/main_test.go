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
	os.Exit(m.Run())
}
