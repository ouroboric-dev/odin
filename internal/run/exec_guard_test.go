package run

import "testing"

// A run.json is data: a process group of 1 or less would make the kill signal every process the user owns.
func TestAnExecRecordWithAProcessGroupOfOneOrLessIsRefused(t *testing.T) {
	for _, pgid := range []int{-1, 0, 1} {
		p := &ExecProcess{Node: "build", PGID: pgid}
		if err := p.Check(); err == nil {
			t.Errorf("Check() accepted pgid %d", pgid)
		}
		if err := StopLeftoverProcess(p); err == nil {
			t.Errorf("StopLeftoverProcess accepted pgid %d", pgid)
		}
	}
	if err := (&ExecProcess{Node: "build", PGID: 2}).Check(); err != nil {
		t.Errorf("Check() = %v for a pgid of 2", err)
	}
}
