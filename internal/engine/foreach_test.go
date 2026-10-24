package engine

import (
	"context"
	"encoding/json"
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
		pack: "checks:\n  unit: [sh, -c, \"test -f " + file + "\"]\nleaf:\n  description: the " + name + " leaf\n  template: templates/leaf\n  first_commit: [LICENSE]\n  implementer: prompts/implementer.md\n  checks: [unit]\n",
		files: map[string]string{"templates/leaf/LICENSE": "licence\n", "templates/leaf/AGENTS.md": "rules of " + name + "\n", "templates/leaf/docs/architecture.md": "# " + name + "\n",
			"prompts/implementer.md": prompt + "\n", "p.md": "Graph prompt.\n"}}
}

const taskShape = `  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection, refusal], output: ygg.plan/v1 }
  - { id: ready, kind: tasks, from: plan }
  - id: tasks
    kind: foreach
    over: tasks
    nodes:
      - { id: implement, kind: loop, realm: leaf, role: implementer, input: [rejection, refusal], output: ygg.implement/v1 }
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
	plan := planOf(taskOf("p-T001", "repos/api", "S"), taskOf("p-T002", "repos/web", "M", "p-T001"))
	return startRealms(t, realmsSetup{mode: mode, nodes: taskShape, script: withPlan(plan, "steps:\n"+script),
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"}})
}

func TestEveryTaskRunsItsShapeInItsLeafWithTheRecipeOfItsRealm(t *testing.T) {
	r := galleryRun(t, "key-points", writeStep("p-t001-implement", "api.txt")+writeStep("p-t002-implement", "web.txt"))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := len(r.Nodes()); got != 2+2*4 || !r.State.Expanded["tasks"] {
		t.Errorf("the run has %d nodes (expanded %v); want the plan and tasks nodes and a shape of four for each of two tasks", got, r.State.Expanded)
	}
	if got := promptOf(t, r, "p-t001-implement-1"); !strings.Contains(got, "API implementer prompt.") || strings.Contains(got, "Web implementer") {
		t.Errorf("task 1's implementer was given:\n%s", got)
	}
	if got := promptOf(t, r, "p-t002-implement-1"); !strings.Contains(got, "Web implementer prompt.") {
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
	if got := strings.Join(taskEvents(t, r), ", "); got != "task_started p-t001, task_done p-t001, task_started p-t002, task_done p-t002" {
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
	r := galleryRun(t, "step-by-step", writeStep("p-t001-implement", "api.txt")+writeStep("p-t002-implement", "web.txt"))
	ctx := context.Background()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t001-feature-gate" {
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
	if len(r.Nodes()) != 10 || r.Nodes()[r.State.Position].ID != "p-t001-feature-gate" {
		t.Fatalf("after Open: %d nodes, position %d", len(r.Nodes()), r.State.Position)
	}
	if err := Approve(ctx, r, "p-t001-feature-gate", Note(r, "p-t001-feature-gate", "")); err != nil {
		t.Fatal(err)
	}
	out, err = Advance(ctx, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t002-feature-gate" {
		t.Fatalf("Advance() = %+v, %v; want task 1 done and a stop at task 2's feature gate", out, err)
	}
	if r.State.Features[0].Status != schema.TaskDone || r.State.Features[1].Status != schema.TaskDoing {
		t.Errorf("statuses: %+v", r.State.Features)
	}
	if err := Approve(ctx, r, "p-t002-feature-gate", Note(r, "p-t002-feature-gate", "")); err != nil {
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
	r := startRealms(t, realmsSetup{nodes: taskShape, script: withPlan(planOf(taskOf("p-T001", "repos/docs", "S")), "steps: {}\n"), realms: []realmFiles{lead, bare},
		leaves: map[string]string{"repos/web": "midgard-react", "repos/docs": "notes"}})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "offers no leaf recipe") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
	if r.State.Expanded["tasks"] {
		t.Error("a failed expansion was recorded")
	}
}

func TestTheTaskIsInThePromptOfItsImplementerAndItsReviewer(t *testing.T) {
	shape := strings.Replace(taskShape, "      - { id: implement, kind: loop, realm: leaf, role: implementer, input: [rejection, refusal], output: ygg.implement/v1 }\n",
		"      - { id: implement, kind: loop, realm: leaf, role: implementer, reviewers: [ { id: spec, role: reviewer, prompt: p.md, input: [task], output: ygg.review/v1 } ], input: [task, rejection, refusal], output: ygg.implement/v1 }\n", 1)
	task := schema.PlanTask{ID: "p-T001", Title: "the API stores galleries", Criteria: []string{"a gallery can be created", "a name is required"},
		NamedTest: "repos/api/gallery_test.go TestCreate", Layer: "repos/api", Size: "S", DependsOn: []string{}, Text: "Store galleries."}
	script := "steps:\n" + writeStep("p-t001-implement", "api.txt") + "  p-t001-implement-spec:\n    - result: { status: ok, summary: no findings, report: [] }\n"
	r := startRealms(t, realmsSetup{nodes: shape, script: withPlan(planOf(task), script),
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API.")},
		leaves: map[string]string{"repos/api": "helheim-go"}})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	for _, stem := range []string{"p-t001-implement-1", "p-t001-implement-spec-1"} {
		got := promptOf(t, r, stem)
		for _, want := range []string{"## Input: task", "p-T001: the API stores galleries", "You work in the leaf repos/api.", "- a gallery can be created", "- a name is required",
			"The test that proves it: repos/api/gallery_test.go TestCreate"} {
			if !strings.Contains(got, want) {
				t.Errorf("the prompt of %s lacks %q:\n%s", stem, want, got)
			}
		}
	}
}

// A task's words reach its prompt whole: the plan's schema and the readiness check bound them, and Odin never cuts
// one (B-176).
func TestATasksWordsAreWholeInAPrompt(t *testing.T) {
	r := startRealms(t, realmsSetup{nodes: taskShape, script: "steps: {}\n", realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web.")},
		leaves: map[string]string{"repos/web": "midgard-react"}})
	title, criterion, test, text := strings.Repeat("t", 200), strings.Repeat("c", 2000), strings.Repeat("n", 500), strings.Repeat("w", schema.MaxTaskText)
	f := schema.Feature{ID: "p-T001", Title: title, Layer: "repos/web", Test: test, Text: text}
	for i := 0; i < 5; i++ {
		f.Criteria = append(f.Criteria, criterion)
	}
	r.State.Features = []schema.FeatureRecord{{Feature: f, Status: schema.TaskTodo}}
	got := taskInput(r, schema.Node{Task: "p-t001"})
	for _, want := range []string{"p-T001: " + title + "\n", "- " + criterion + "\n", "The test that proves it: " + test + "\n", "### What the plan says\n\n" + text + "\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the task block lacks a field whole (%d bytes of it)", len(want))
		}
	}
	if n := strings.Count(got, "- "+criterion+"\n"); n != 5 {
		t.Errorf("the block holds %d of the 5 criteria", n)
	}
	if taskInput(r, schema.Node{}) != "" || taskInput(r, schema.Node{Task: "nobody"}) != "" {
		t.Error("a node with no task, or an unknown one, gets a task block")
	}
}

// replanShape plans in repos/web, reads the plan, builds its tasks, and ends at a node after the tasks
// whose rejection or refusal goes back to the planner.
const replanShape = "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/web, input: [rejection, refusal], output: ygg.plan/v1 }\n" +
	"  - { id: ready, kind: tasks, from: plan }\n" + "  - id: tasks\n    kind: foreach\n    over: tasks\n    nodes:\n" +
	"      - { id: implement, kind: loop, realm: leaf, role: implementer, input: [rejection, refusal], output: ygg.implement/v1 }\n      - { id: land, kind: merge, realm: leaf }\n"

// replanRun is a run whose planner writes a plan of one task first, and of two other tasks the second time.
func replanRun(t *testing.T, tail, pack string) *run.Run {
	t.Helper()
	lead := recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt.")
	lead.pack = strings.Replace(lead.pack, "checks:\n", "checks:\n"+pack, 1)
	script := planScript(planOf(taskOf("p-T001", "repos/web", "S")),
		planOf(taskOf("q-T001", "repos/web", "S"), taskOf("q-T002", "repos/web", "S", "q-T001"))) +
		writeStep("p-t001-implement", "web.txt") + writeStep("q-t001-implement", "web.txt") + writeStep("q-t002-implement", "web2.txt")
	return startRealms(t, realmsSetup{nodes: replanShape + tail, script: script, realms: []realmFiles{lead},
		leaves: map[string]string{"repos/web": "midgard-react"}})
}

// checkReplanned checks the run built the second plan's tasks, with the nodes of that plan, and not the first's.
func checkReplanned(t *testing.T, r *run.Run) {
	t.Helper()
	var ids []string
	for _, n := range r.Nodes() {
		ids = append(ids, n.ID)
	}
	if got := strings.Join(ids, " "); !strings.Contains(got, "q-t001-implement q-t001-land q-t002-implement q-t002-land") || strings.Contains(got, "p-t001") {
		t.Errorf("the run walks %s; want the nodes of the new plan's tasks", got)
	}
	if r.State.Attempts["q-t001-implement"] != 1 || r.State.Attempts["q-t002-implement"] != 1 || r.State.Attempts["p-t001-implement"] != 1 {
		t.Errorf("attempts = %v; want each task's implementer once, the first plan's before the rejection", r.State.Attempts)
	}
	for _, f := range r.State.Features {
		if f.Status != schema.TaskDone {
			t.Errorf("task %s is %s", f.ID, f.Status)
		}
	}
}

func TestARejectionBeforeTheTasksNodeExpandsTheTasksAgainAgainstTheNewPlan(t *testing.T) {
	r := replanRun(t, "  - { id: final, kind: gate, gate: design, reruns: plan }\n", "")
	ctx := context.Background()
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusStopped || out.Gate != "final" {
		t.Fatalf("Advance() = %+v, %v; want a stop at final", out, err)
	}
	if err := Reject(ctx, r, "final", Reason("plan two tasks instead")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusStopped || out.Gate != "final" {
		t.Fatalf("Advance() = %+v, %v; want a stop at final again", out, err)
	}
	checkReplanned(t, r)
	want := r.Nodes()
	id, project := r.State.ID, r.Project
	r.Close()
	again, err := run.Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v; a resume must make the nodes the run walked", err)
	}
	defer again.Close()
	got := again.Nodes()
	if len(got) != len(want) || got[again.State.Position].ID != "final" {
		t.Fatalf("after Open: %d nodes (want %d), position %d", len(got), len(want), again.State.Position)
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Errorf("node %d is %s after Open, %s before", i, got[i].ID, want[i].ID)
		}
	}
}

func TestARefusalSentBackBeforeTheTasksNodeExpandsTheTasksAgainAgainstTheNewPlan(t *testing.T) {
	r := replanRun(t, "  - { id: tail, kind: merge, repo: repos/web, checks: [late], back_to: plan }\n", "  late: [test, -f, web2.txt]\n")
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if r.State.Returns["tail"] != 1 {
		t.Errorf("tail's refusal went back %d times, want once", r.State.Returns["tail"])
	}
	checkReplanned(t, r)
}
