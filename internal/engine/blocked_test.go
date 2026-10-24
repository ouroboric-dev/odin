package engine

import (
	"context"
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
	return blockedRunOf(t, mode, planOf(taskOf("p-T001", "repos/api", "S"), taskOf("p-T002", "repos/web", "M", "p-T001"),
		taskOf("p-T003", "repos/web", "S", "p-T002"), taskOf("p-T004", "repos/api", "S", "p-T001")))
}

// blockedRunOf is blockedRun with the plan given.
func blockedRunOf(t *testing.T, mode, plan string) *run.Run {
	t.Helper()
	idle := "    - result: { status: ok, summary: did nothing, output: '{\"summary\": \"done\"}' }\n"
	script := writeStep("p-t001-implement", "api.txt") + writeStep("p-t004-implement", "api2.txt") +
		"  p-t002-implement:\n" + strings.Repeat(idle, 3) + writeStep("p-t003-implement", "web2.txt")
	return startRealms(t, realmsSetup{mode: mode, nodes: taskShape, script: withPlan(plan, "steps:\n"+script),
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"}})
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
	want := map[string]schema.TaskStatus{"p-T001": schema.TaskDone, "p-T002": schema.TaskBlocked, "p-T003": schema.TaskBlocked, "p-T004": schema.TaskDone}
	for id, st := range want {
		if status[id] != st {
			t.Errorf("task %s is %s, want %s (all: %v)", id, status[id], st, status)
		}
	}
	if why := r.State.Features[1].Why; !strings.Contains(why, "attempts-exhausted") || !strings.Contains(why, "refs/ygg/"+r.State.ID+"/blocked/p-t002") {
		t.Errorf("task 2's reason = %q", why)
	}
	if why := r.State.Features[2].Why; !strings.Contains(why, "depends on p-T002, which is blocked") {
		t.Errorf("task 3's reason = %q", why)
	}
	if r.State.Attempts["p-t003-implement"] != 0 {
		t.Error("a task that depends on a blocked task was started")
	}
	if got := strings.Join(taskEvents(t, r), ", "); got != "task_started p-t001, task_done p-t001, task_started p-t002, task_blocked p-t002, task_blocked p-t003, task_started p-t004, task_done p-t004" {
		t.Errorf("task events = %s", got)
	}
	web, _ := r.Leaf("repos/web")
	main, _ := web.Rev(context.Background(), "main")
	if head, _ := web.Head(context.Background()); head != main || r.State.Leaves["repos/web"].Checkpoint != main {
		t.Errorf("repos/web is at %s with checkpoint %s, want main %s: the blocked task's work is not left on the run's branch", head, r.State.Leaves["repos/web"].Checkpoint, main)
	}
	if kept, err := web.Git(context.Background(), "rev-parse", "refs/ygg/"+r.State.ID+"/blocked/p-t002"); err != nil || kept == "" {
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
	if !strings.Contains(text, `"ygg.result":"done, with 2 task(s) blocked: p-T002, p-T003"`) || !strings.Contains(text, `"ygg.result":"blocked"`) {
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
					if err := Approve(context.Background(), r, out.Gate, Note(out.Gate, "")); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if out.Status != run.StatusStopped || out.Gate != "p-t002-implement" || r.State.PendingGate != "escalation" {
					t.Errorf("%s: Advance() = %+v, pending %s; want a stop at the escalation of task 2's loop", mode, out, r.State.PendingGate)
				}
				break
			}
			continue
		}
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t002-implement" || r.State.PendingGate != "escalation" {
			t.Errorf("%s: Advance() = %+v, %v; want a stop at the escalation of task 2's loop", mode, out, err)
		}
		if r.State.Features[1].Status == schema.TaskBlocked {
			t.Errorf("%s: a task was given up with an operator there to decide", mode)
		}
	}
}

func TestATaskDoneJustBeforeTasksThatAreSkippedIsStillDone(t *testing.T) {
	// T3 is the last task that runs: T4 comes after it and depends on the blocked T2, so the walk skips it.
	r := blockedRunOf(t, "unattended", planOf(taskOf("p-T001", "repos/api", "S"), taskOf("p-T002", "repos/web", "M", "p-T001"), taskOf("p-T003", "repos/api", "S", "p-T001"), taskOf("p-T004", "repos/web", "S", "p-T002")))
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	got := map[string]schema.TaskStatus{}
	for _, f := range r.State.Features {
		got[f.ID] = f.Status
	}
	if got["p-T003"] != schema.TaskDone || got["p-T004"] != schema.TaskBlocked || got["p-T002"] != schema.TaskBlocked || got["p-T001"] != schema.TaskDone {
		t.Errorf("statuses = %v", got)
	}
}

func TestATaskBlockedAfterItsMergeLandedPutsTheLeafsMainBackToWhereTheTaskFoundIt(t *testing.T) {
	// The task's work passes its loop and lands on the leaf's main; the pointer's acceptance refuses it and
	// sends it back, and the loop then gives up. Its work must not stay on main for a later task to build on.
	shape := strings.Replace(taskShape, "realm: leaf, role: implementer, input:", "realm: leaf, role: implementer, attempts: 2, input:", 1)
	shape = strings.Replace(shape, "      - { id: move, kind: pointer, realm: leaf }\n", "      - { id: move, kind: pointer, realm: leaf, acceptance: [accept], back_to: implement }\n", 1)
	lead := recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt.")
	lead.pack = strings.Replace(lead.pack, "checks:\n", "checks:\n  accept: [sh, -c, \"test -f repos/web/never.txt\"]\n", 1)
	script := "steps:\n  p-t001-implement:\n" +
		"    - actions: [ { write: { path: web.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote web.txt, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ { remove: web.txt } ]\n      result: { status: ok, summary: removed web.txt, output: '{\"summary\": \"done\"}' }\n" +
		"    - result: { status: ok, summary: did nothing, output: '{\"summary\": \"done\"}' }\n    - result: { status: ok, summary: did nothing, output: '{\"summary\": \"done\"}' }\n"
	r := startRealms(t, realmsSetup{mode: "unattended", nodes: shape, script: withPlan(planOf(taskOf("p-T001", "repos/web", "S")), script), realms: []realmFiles{lead},
		leaves: map[string]string{"repos/web": "midgard-react"}})
	ctx := context.Background()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want the run done, with the task given up", out, err)
	}
	if r.State.Returns["p-t001-move"] != 1 || r.State.Features[0].Status != schema.TaskBlocked {
		t.Fatalf("returns %d, task %s; want the pointer's refusal sent back once and the task blocked", r.State.Returns["p-t001-move"], r.State.Features[0].Status)
	}
	// The leaf joined the run with the plan, at its main then, and the task is the first in it.
	web, _ := r.Leaf("repos/web")
	before := r.State.Leaves["repos/web"].Base
	if main, _ := web.Rev(ctx, "main"); main != before {
		t.Errorf("repos/web's main is at %s, want %s, where it was before the task: the work the pointer refused is still on main", main, before)
	}
	if head, _ := web.Head(ctx); head != before || r.State.Leaves["repos/web"].Checkpoint != before {
		t.Errorf("repos/web is at %s with checkpoint %s, want %s", head, r.State.Leaves["repos/web"].Checkpoint, before)
	}
	if _, err := web.Git(ctx, "show", "main:web.txt"); err == nil {
		t.Error("main holds web.txt, which the pointer refused")
	}
	ref := "refs/ygg/" + r.State.ID + "/blocked/p-t001"
	if landed, err := web.Git(ctx, "log", "--format=%H", "-1", ref, "--", "web.txt"); err != nil || landed == "" {
		t.Errorf("the work the task landed is not kept on %s: %q, %v", ref, landed, err)
	}
}
