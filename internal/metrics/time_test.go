package metrics

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

func minutes(d Duration) float64 { return time.Duration(d).Minutes() }

// Spec §10.2: wall time excludes approval waits. A failed run waiting for a person to resume it is not the
// factory's time either, and the report keeps the two apart.
func TestWallTimeLeavesOutApprovalWaitsAndTimeTheRunWasStopped(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(10, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "design"})
	f.log(10, gungnir.Event{Name: gungnir.EventGatePending, Node: "design-gate", Gate: "design-gate"})
	f.log(70, gungnir.Event{Name: gungnir.EventGateApproved, Node: "design-gate", Gate: "design-gate", Note: "go ahead"})
	f.log(75, gungnir.Event{Name: gungnir.EventRunFailed, Result: "node plan: context canceled"})
	f.log(105, gungnir.Event{Name: gungnir.EventRunResumed})
	f.log(115, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "plan"})
	f.log(120, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	r := mustCollect(t, f.done())
	got := r.Time
	if minutes(got.Elapsed) != 120 || minutes(got.ApprovalWait) != 60 || minutes(got.Stopped) != 30 || minutes(got.Active) != 30 {
		t.Errorf("elapsed %v, approval wait %v, stopped %v, active %v; want 120, 60, 30 and 30 minutes",
			got.Elapsed, got.ApprovalWait, got.Stopped, got.Active)
	}
}

func TestAProcessKilledWithoutALedgerLineLeavesAGapThatTheResumeClosesAndTheReportCallsStopped(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(20, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "implement"})
	f.log(50, gungnir.Event{Name: gungnir.EventRunResumed})
	f.log(60, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	got := mustCollect(t, f.done()).Time
	if minutes(got.Stopped) != 30 || minutes(got.Active) != 30 || got.ApprovalWait != 0 {
		t.Errorf("stopped %v, active %v, approval wait %v; want 30, 30 and 0 minutes", got.Stopped, got.Active, got.ApprovalWait)
	}
}

func TestARunStillWaitingAtAGateCountsTheWaitUpToItsLastLineNotBeyond(t *testing.T) {
	f := newFake(t)
	f.State.Status = "stopped"
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(20, gungnir.Event{Name: gungnir.EventGatePending, Node: "design-gate", Gate: "design-gate"})
	got := mustCollect(t, f.done()).Time
	if minutes(got.Elapsed) != 20 || got.ApprovalWait != 0 || minutes(got.Active) != 20 {
		t.Errorf("elapsed %v, approval wait %v, active %v; want 20, 0 and 20 minutes", got.Elapsed, got.ApprovalWait, got.Active)
	}
}

func TestTimesAndDurationsAreWrittenAsWholeMilliseconds(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(2, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	b, err := json.Marshal(mustCollect(t, f.done()).Time)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"elapsed_ms":120000,"active_ms":120000,"approval_wait_ms":0,"stopped_ms":0}`; string(b) != want {
		t.Errorf("Time = %s, want %s", b, want)
	}
}
