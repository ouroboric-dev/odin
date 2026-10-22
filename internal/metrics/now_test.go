package metrics

import (
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

func TestCurrentNamesTheNodeThatIsRunningAndItsAttempt(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	f.log(5, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "plan", Attempt: 1})
	f.log(6, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "g-t1-implement", Attempt: 2, Role: "implementer"})
	now, err := Current(f.done())
	if err != nil {
		t.Fatal(err)
	}
	if now.Node != "g-t1-implement" || now.Attempt != 2 || now.Role != "implementer" || !now.Since.Equal(t0.Add(6*time.Minute)) || now.Between {
		t.Errorf("Current() = %+v", now)
	}
}

func TestCurrentBetweenNodesSaysWhatFinishedLast(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	f.log(5, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "plan", Attempt: 1})
	now, err := Current(f.done())
	if err != nil || !now.Between || now.Node != "plan" || now.Attempt != 1 {
		t.Errorf("Current() = %+v, %v; want between nodes, after plan", now, err)
	}
	empty := newFake(t)
	if now, err := Current(empty.done()); err != nil || now.Node != "" {
		t.Errorf("a run with no line: Current() = %+v, %v", now, err)
	}
}

// The live view reads a ledger another process is writing: a half-written last line is not an error.
func TestCurrentIgnoresALastLineThatIsStillBeingWritten(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	dir := f.done()
	appendRaw(t, dir, `{"schema":"ygg.ledger/v1","seq":3,"ti`)
	if now, err := Current(dir); err != nil || now.Node != "plan" {
		t.Errorf("Current() = %+v, %v", now, err)
	}
}
