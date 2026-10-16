package run

import "testing"

// SetProcessIDsForTest makes Prepare see the process as running with these ids until the test ends.
// Tests that prepare a sandbox use it with a non-root pair, so they pass whoever runs the suite.
func SetProcessIDsForTest(t testing.TB, uid, gid int) {
	t.Helper()
	old := processIDs
	processIDs = func() (int, int) { return uid, gid }
	t.Cleanup(func() { processIDs = old })
}
