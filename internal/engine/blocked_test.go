package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// A plan of four tasks: 1 builds the api, 2 the web leaf on it, 3 depends on 2, and 4 builds more of the
// api and depends on nothing. Task 2's implementer never writes what its check wants.
func blockedRun(t *testing.T, mode string) *run.Run {
	t.Helper()
	idle := "    - result: { status: ok, summary: did nothing }\n"
	script := writeStep("p-t1-implement", "api.txt") + writeStep("p-t4-implement", "api2.txt") +
		"  p-t2-implement:\n" + strings.Repeat(idle, 3) + writeStep("p-t3-implement", "web2.txt")
	r := startRealms(t, realmsSetup{mode: mode, nodes: taskShape, script: "steps:\n" + script,
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"}})
	plan := "# Tasks\n\n" + taskMD("p-T1", "repos/api", "S", "nothing") + taskMD("p-T2", "repos/web", "M", "p-T1") +
		taskMD("p-T3", "repos/web", "S", "p-T2") + taskMD("p-T4", "repos/api", "S", "p-T1")
	if err := os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAnUnattendedRunGivesUpATaskWhoseLoopGaveUpAndGoesOnWithTheRest(t *testing.T) {
	r := blockedRun(t, "unattended")
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want the run done, with a task given up", out, err)
	}
	status := map[string]schema.TaskStatus{}
	for _, f := range r.State.Features {
		status[f.ID] = f.Status
	}
	want := map[string]schema.TaskStatus{"p-T1": schema.TaskDone, "p-T2": schema.TaskBlocked, "p-T3": schema.TaskBlocked, "p-T4": schema.TaskDone}
	for id, st := range want {
		if status[id] != st {
			t.Errorf("task %s is %s, want %s (all: %v)", id, status[id], st, status)
		}
	}
	if why := r.State.Features[1].Why; !strings.Contains(why, "attempts-exhausted") || !strings.Contains(why, "refs/ygg/"+r.State.ID+"/blocked/p-t2") {
		t.Errorf("task 2's reason = %q", why)
	}
	if why := r.State.Features[2].Why; !strings.Contains(why, "depends on p-T2, which is blocked") {
		t.Errorf("task 3's reason = %q", why)
	}
	if r.State.Attempts["p-t3-implement"] != 0 {
		t.Error("a task that depends on a blocked task was started")
	}
	if got := strings.Join(taskEvents(t, r), ", "); got != "task_started p-t1, task_done p-t1, task_started p-t2, task_blocked p-t2, task_blocked p-t3, task_started p-t4, task_done p-t4" {
		t.Errorf("task events = %s", got)
	}
	web, _ := r.Leaf("repos/web")
	main, _ := web.Rev(context.Background(), "main")
	if head, _ := web.Head(context.Background()); head != main || r.State.Leaves["repos/web"].Checkpoint != main {
		t.Errorf("repos/web is at %s with checkpoint %s, want main %s: the blocked task's work is not left on the run's branch", head, r.State.Leaves["repos/web"].Checkpoint, main)
	}
	if kept, err := web.Git(context.Background(), "rev-parse", "refs/ygg/"+r.State.ID+"/blocked/p-t2"); err != nil || kept == "" {
		t.Errorf("the blocked task's work is not kept on a ref: %q, %v", kept, err)
	}
	api, _ := r.Leaf("repos/api")
	if body, err := api.Git(context.Background(), "show", "main:api2.txt"); err != nil || body != "done" {
		t.Errorf("the task that doesn't depend on the blocked one did not land: %q, %v", body, err)
	}
	f, err := schema.LoadFeatures(filepath.Join(r.Project, ".factory", "features.json"))
	if err != nil || f.Tasks[1].Status != schema.TaskBlocked || f.Tasks[1].Why == "" {
		t.Errorf("features.json = %+v, %v", f, err)
	}
	text := ledgerText(t, r)
	if !strings.Contains(text, `"ygg.result":"done, with 2 task(s) blocked: p-T2, p-T3"`) || !strings.Contains(text, `"ygg.result":"blocked"`) {
		t.Errorf("the ledger doesn't list the blocked tasks:\n%s", text)
	}
}

func TestOtherModesStopAtTheLoopThatGaveUp(t *testing.T) {
	for _, mode := range []string{"step-by-step", "key-points"} {
		r := blockedRun(t, mode)
		if mode == "step-by-step" {
			// every task's feature gate stops in this mode: approve them as they come
			for {
				out, err := Advance(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				if out.Status == run.StatusStopped && strings.HasSuffix(out.Gate, "feature-gate") {
					if err := Approve(context.Background(), r, out.Gate, ""); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if out.Status != run.StatusStopped || out.Gate != "p-t2-implement" || r.State.PendingGate != "escalation" {
					t.Errorf("%s: Advance() = %+v, pending %s; want a stop at the escalation of task 2's loop", mode, out, r.State.PendingGate)
				}
				break
			}
			continue
		}
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t2-implement" || r.State.PendingGate != "escalation" {
			t.Errorf("%s: Advance() = %+v, %v; want a stop at the escalation of task 2's loop", mode, out, err)
		}
		if r.State.Features[1].Status == schema.TaskBlocked {
			t.Errorf("%s: a task was given up with an operator there to decide", mode)
		}
	}
}
