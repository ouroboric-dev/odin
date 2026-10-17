package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// recipeRealm is a realm with a leaf recipe: a template, an implementer prompt and one check, which passes
// when the leaf holds file.
func recipeRealm(name, version, file, prompt string) realmFiles {
	return realmFiles{name: name, version: version,
		pack:  "checks:\n  unit: [sh, -c, \"test -f " + file + "\"]\nleaf:\n  description: the " + name + " leaf\n  template: templates/leaf\n  first_commit: [LICENSE]\n  implementer: prompts/implementer.md\n  checks: [unit]\n",
		files: map[string]string{"templates/leaf/LICENSE": "licence\n", "prompts/implementer.md": prompt + "\n", "p.md": "Lead prompt.\n"}}
}

const taskShape = `  - { id: ready, kind: tasks, from: tasks.md }
  - id: tasks
    kind: foreach
    over: tasks
    nodes:
      - { id: implement, kind: loop, realm: leaf, role: implementer }
      - { id: feature-gate, kind: gate, gate: feature }
      - { id: land, kind: merge, realm: leaf }
      - { id: move, kind: pointer, realm: leaf }
`

// writeStep is the script entry of an implementer that writes file.
func writeStep(node, file string) string {
	return "  " + node + ":\n    - actions: [ { write: { path: " + file + ", content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote " + file + " }\n"
}

func galleryRun(t *testing.T, mode, script string) *run.Run {
	t.Helper()
	r := startRealms(t, realmsSetup{mode: mode, nodes: taskShape, script: "steps:\n" + script,
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"}})
	plan := "# Tasks\n\n" + taskMD("p-T1", "repos/api", "S", "nothing") + taskMD("p-T2", "repos/web", "M", "p-T1")
	if err := os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEveryTaskRunsItsShapeInItsLeafWithTheRecipeOfItsRealm(t *testing.T) {
	r := galleryRun(t, "key-points", writeStep("p-t1-implement", "api.txt")+writeStep("p-t2-implement", "web.txt"))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := len(r.Nodes()); got != 1+2*4 || !r.State.Expanded["tasks"] {
		t.Errorf("the run has %d nodes (expanded %v); want the tasks node and a shape of four for each of two tasks", got, r.State.Expanded)
	}
	if got := promptOf(t, r, "p-t1-implement-1"); !strings.Contains(got, "API implementer prompt.") || strings.Contains(got, "Web implementer") {
		t.Errorf("task 1's implementer was given:\n%s", got)
	}
	if got := promptOf(t, r, "p-t2-implement-1"); !strings.Contains(got, "Web implementer prompt.") {
		t.Errorf("task 2's implementer was given:\n%s", got)
	}
	for leaf, file := range map[string]string{"repos/api": "api.txt", "repos/web": "web.txt"} {
		l, err := r.Leaf(leaf)
		if err != nil {
			t.Fatal(err)
		}
		if body, err := l.Git(context.Background(), "show", "main:"+file); err != nil || body != "done" {
			t.Errorf("%s: main:%s = %q, %v; the task's work is landed on the leaf's main", leaf, file, body, err)
		}
		if rec := r.State.Leaves[leaf]; rec.Branch != run.Branch(r.State.ID) {
			t.Errorf("%s record = %+v", leaf, rec)
		}
	}
	for _, f := range r.State.Features {
		if f.Status != schema.TaskDone {
			t.Errorf("task %s is %s", f.ID, f.Status)
		}
	}
	saved, err := schema.LoadFeatures(filepath.Join(r.Project, ".factory", "features.json"))
	if err != nil || saved.Tasks[0].Status != schema.TaskDone || saved.Tasks[1].Status != schema.TaskDone {
		t.Errorf("features.json = %+v, %v", saved, err)
	}
	if got := strings.Join(taskEvents(t, r), ", "); got != "task_started p-t1, task_done p-t1, task_started p-t2, task_done p-t2" {
		t.Errorf("task events = %s; task 1 starts and is done before task 2 starts", got)
	}
}

// taskEvents lists the task_started, task_done and task_blocked lines of the ledger, with their task.
func taskEvents(t *testing.T, r *run.Run) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(ledgerText(t, r)), "\n") {
		var e struct {
			Event string `json:"event"`
			Task  string `json:"ygg.task.id"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(e.Event, "task_") {
			out = append(out, e.Event+" "+e.Task)
		}
	}
	return out
}

func TestAResumeMakesTheSameNodesAndGoesOnWhereTheRunStopped(t *testing.T) {
	r := galleryRun(t, "step-by-step", writeStep("p-t1-implement", "api.txt")+writeStep("p-t2-implement", "web.txt"))
	ctx := context.Background()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t1-feature-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at task 1's feature gate", out, err)
	}
	if r.State.Features[0].Status != schema.TaskDoing || r.State.Features[1].Status != schema.TaskTodo {
		t.Errorf("statuses while task 1 waits: %+v", r.State.Features)
	}
	id, project := r.State.ID, r.Project
	r.Close()
	r, err = run.Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer r.Close()
	if len(r.Nodes()) != 9 || r.Nodes()[r.State.Position].ID != "p-t1-feature-gate" {
		t.Fatalf("after Open: %d nodes, position %d", len(r.Nodes()), r.State.Position)
	}
	if err := Approve(ctx, r, "p-t1-feature-gate", ""); err != nil {
		t.Fatal(err)
	}
	out, err = Advance(ctx, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t2-feature-gate" {
		t.Fatalf("Advance() = %+v, %v; want task 1 done and a stop at task 2's feature gate", out, err)
	}
	if r.State.Features[0].Status != schema.TaskDone || r.State.Features[1].Status != schema.TaskDoing {
		t.Errorf("statuses: %+v", r.State.Features)
	}
	if err := Approve(ctx, r, "p-t2-feature-gate", ""); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Features[1].Status != schema.TaskDone {
		t.Errorf("the last task, whose last node is a move, is %s", r.State.Features[1].Status)
	}
}

func TestAForeachNodeWithNoPlanFailsInsteadOfDoingNothing(t *testing.T) {
	r := startRealms(t, realmsSetup{nodes: taskShape[strings.Index(taskShape, "  - id: tasks"):], script: "steps: {}\n",
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web.")}, leaves: map[string]string{"repos/web": "midgard-react"}})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "a tasks node must come before it") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}

func TestATaskInALeafWhoseRealmOffersNoRecipeFailsTheNode(t *testing.T) {
	lead := recipeRealm("midgard-react", "0.1.0", "web.txt", "Web.")
	bare := realmFiles{name: "notes", version: "0.1.0"}
	r := startRealms(t, realmsSetup{nodes: taskShape, script: "steps: {}\n", realms: []realmFiles{lead, bare},
		leaves: map[string]string{"repos/web": "midgard-react", "repos/docs": "notes"}})
	plan := "# Tasks\n\n" + taskMD("p-T1", "repos/docs", "S", "nothing")
	os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(plan), 0o644)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "offers no leaf recipe") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
	if r.State.Expanded["tasks"] {
		t.Error("a failed expansion was recorded")
	}
}
