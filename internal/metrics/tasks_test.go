package metrics

import (
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

func TestPerTaskAndPerLeafUsageFollowTheTaskTagOfTheLedgerLines(t *testing.T) {
	f := newFake(t).features(
		feature("gallery-T1", "repos/api", schema.TaskDone, ""),
		feature("gallery-T2", "repos/web", schema.TaskBlocked, "attempts-exhausted in implement"),
		feature("gallery-T3", "repos/web", schema.TaskTodo, ""))
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	a := session("gallery-t1-implement", "implementer", "claude-code", "sonnet", 100, 10, 3, 0.05, true)
	a.Task = "gallery-t1"
	b := session("gallery-t2-implement", "implementer", "claude-code", "sonnet", 200, 20, 6, 0.09, true)
	b.Task = "gallery-t2"
	c := session("gallery-t2-implement", "implementer", "claude-code", "sonnet", 200, 20, 6, 0.09, true)
	c.Task, c.Attempt = "gallery-t2", 2
	f.log(2, a).log(4, b).log(6, c)
	f.log(7, gungnir.Event{Name: gungnir.EventTaskBlocked, Task: "gallery-t2", Result: "blocked"})
	r := mustCollect(t, f.done())
	if len(r.Tasks) != 3 {
		t.Fatalf("Tasks = %+v, want the plan's three", r.Tasks)
	}
	t2 := r.Tasks[1]
	if t2.ID != "gallery-T2" || t2.Status != "blocked" || t2.Why != "attempts-exhausted in implement" || t2.Usage.Turns != 12 || t2.Leaf != "repos/web" {
		t.Errorf("task 2 = %+v", t2)
	}
	if got := r.Leaves["repos/web"]; got.Tasks != 2 || got.Blocked != 1 || got.Done != 0 || got.Usage.Turns != 12 {
		t.Errorf("leaf repos/web = %+v", got)
	}
	if got := r.Leaves["repos/api"]; got.Tasks != 1 || got.Done != 1 || !near(got.Usage.CostUSD, 0.05) {
		t.Errorf("leaf repos/api = %+v", got)
	}
	if len(r.BlockedTasks) != 1 || r.BlockedTasks[0].Task != "gallery-T2" {
		t.Errorf("BlockedTasks = %+v", r.BlockedTasks)
	}
}
