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

// taskOf is one task of a test's plan, ready unless the test changes a field.
func taskOf(id, layer, size string, deps ...string) schema.PlanTask {
	return schema.PlanTask{ID: id, Title: "task " + id, Criteria: []string{"It works.", "It is tested."}, NamedTest: "TestIt",
		Layer: layer, Size: size, DependsOn: append([]string{}, deps...), Text: "The task " + id + "."}
}

// planOf is a test's plan of tasks, a ygg.plan/v1 document, as a script's single-quoted output.
func planOf(tasks ...schema.PlanTask) string {
	raw, err := json.Marshal(schema.Plan{Title: "Plan", Slug: "plan", Goal: "The plan of the test.", Decisions: []schema.Decided{}, Risks: []string{}, Tasks: tasks})
	if err != nil {
		panic(err)
	}
	return "'" + strings.ReplaceAll(string(raw), "'", "''") + "'"
}

// planDone is the answer of a test's planner whose tasks do not matter to it, a ygg.plan/v1 document: one task, in
// repos/app.
const planDone = `{"title": "Plan", "slug": "plan", "goal": "The plan of the test.", "decisions": [], "risks": [], "tasks": [{"id": "plan-T001", "title": "Task one", "criteria": ["It works.", "It is tested."], "named_test": "app_test.go:TestOne", "layer": "repos/app", "size": "S", "depends_on": [], "text": "The task of the test."}]}`

// planScript is a planner's answers, one for each attempt.
func planScript(plans ...string) string {
	var b strings.Builder
	b.WriteString("steps:\n  plan:\n")
	for _, p := range plans {
		b.WriteString("    - result: { status: ok, summary: planned, output: " + p + " }\n")
	}
	return b.String()
}

// withPlan is script with the planner's answer, plan, as its first step.
func withPlan(plan, script string) string {
	step := "  plan:\n    - result: { status: ok, summary: planned, output: " + plan + " }\n"
	if script == "steps: {}\n" {
		return "steps:\n" + step
	}
	return strings.Replace(script, "steps:\n", "steps:\n"+step, 1)
}

func quote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
	return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"`
}

// planScratch is a planner in a scratch workspace.
const planScratch = "  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection, refusal], output: ygg.plan/v1 }\n"

const planNodes = "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection, refusal], output: ygg.plan/v1 }\n"

func TestAReadyPlanBecomesFeaturesInTheOrderTheyAreBuilt(t *testing.T) {
	plan := planOf(taskOf("gallery-T002", "repos/app", "M", "gallery-T001"), taskOf("gallery-T001", "repos/app", "S"))
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: plan }\n", planScript(plan))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(r.State.Features) != 2 || r.State.Features[0].ID != "gallery-T001" || r.State.Features[1].ID != "gallery-T002" ||
		r.State.Features[0].Status != schema.TaskTodo || r.State.PlanSource != ".factory/runs/"+r.State.ID+"/outputs/plan.json" || r.State.PlanSHA256 != r.State.Outputs["plan"].SHA256 {
		t.Fatalf("Features = %+v, source %q", r.State.Features, r.State.PlanSource)
	}
	f, err := schema.LoadFeatures(filepath.Join(r.Project, ".factory", "features.json"))
	if err != nil || f.Run != r.State.ID || len(f.Tasks) != 2 || f.Tasks[0].ID != "gallery-T001" || f.Tasks[1].DependsOn[0] != "gallery-T001" {
		t.Errorf("features.json = %+v, %v", f, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"tasks_ready"`) || !strings.Contains(text, "2 task(s) are ready, from the output of plan (.factory/runs/"+r.State.ID+"/outputs/plan.json)") {
		t.Errorf("the ledger lacks tasks_ready:\n%s", text)
	}
}

func TestAPlanThatIsNotReadyGoesBackToThePlannerWithEveryProblem(t *testing.T) {
	notOpenAPI := taskOf("p-T001", "repos/app", "L")
	notOpenAPI.Contract = "openapi: 3.0.3\ninfo: { title: x, version: 1.0.0 }\n"
	bad := planOf(notOpenAPI, taskOf("p-T002", "repos/db", "S", "p-T009"))
	good := planOf(taskOf("p-T001", "repos/app", "S"), taskOf("p-T002", "repos/app", "S", "p-T001"))
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: plan, back_to: plan }\n", planScript(bad, good))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Attempts["plan"] != 2 || r.State.Returns["ready"] != 1 {
		t.Errorf("the planner ran %d times, the refusal went back %d times; want 2 and 1", r.State.Attempts["plan"], r.State.Returns["ready"])
	}
	prompt := promptOf(t, r, "plan-2")
	for _, want := range []string{"## Input: refusal\n\nA later step of the factory refused", "size L: split it", `its contract is not an OpenAPI document: it has no "paths" mapping`, `layer "repos/db" is not a leaf of this project (repos/app)`, `depends on "p-T009"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the planner's second prompt lacks %q:\n%s", want, prompt)
		}
	}
	if first := promptOf(t, r, "plan-1"); !strings.Contains(first, "## Input: refusal\n\nNone.\n") {
		t.Error("the first attempt was told of a refusal that had not happened")
	}
	if len(r.State.Features) != 2 || len(r.State.Refusals) != 0 {
		t.Errorf("features %d, refusals %v; the refusal is spent once the planner's work is accepted", len(r.State.Features), r.State.Refusals)
	}
	text := ledgerText(t, r)
	if !strings.Contains(text, `"event":"node_returned"`) || !strings.Contains(text, "back to plan, 1 of 2") {
		t.Errorf("the ledger lacks node_returned:\n%s", text)
	}
}

func TestARefusalGoesBackOnlyAsOftenAsTheNodeAllows(t *testing.T) {
	bad := planOf(taskOf("p-T001", "repos/app", "L"))
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: plan, back_to: plan, returns: 1 }\n", planScript(bad, bad, bad))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "size L: split it") {
		t.Fatalf("Advance() = %+v, %v; want the run failed with the readiness error", out, err)
	}
	if r.State.Attempts["plan"] != 2 || r.State.Returns["ready"] != 1 {
		t.Errorf("planner %d times, returned %d times; want 2 and 1", r.State.Attempts["plan"], r.State.Returns["ready"])
	}
	if _, err := os.Stat(filepath.Join(r.Project, ".factory", "features.json")); err == nil {
		t.Error("features.json was written for a plan that is not ready")
	}
}

func TestAPlanWithNoBackToFailsTheNode(t *testing.T) {
	untested := taskOf("p-T001", "repos/app", "S")
	untested.NamedTest = " "
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: plan }\n", planScript(planOf(untested)))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "no named test") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}

// The plan is the output Odin kept, as it validated it: a changed file is not read, and a tasks.md is never read.
func TestThePlanIsTheOutputOdinKept(t *testing.T) {
	nodes := planScratch + "  - { id: plan-gate, kind: gate, gate: plan }\n  - { id: ready, kind: tasks, from: plan }\n"
	r := start(t, "key-points", "gates:\n  plan: stop\n", nodes, "steps:\n  plan:\n    - result: { status: ok, output: '"+planDone+"' }\n")
	advanceTo(t, r, "plan-gate")
	changed := strings.Replace(planDone, "repos/app", "repos/elsewhere", 1)
	if err := os.WriteFile(filepath.Join(r.Dir, "outputs", "plan.json"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Approve(context.Background(), r, "plan-gate", Note("plan-gate", "")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the plan can't be read: the output of plan changed since Odin kept it") {
		t.Fatalf("Advance() = %+v, %v; want the changed plan refused", out, err)
	}
	if len(r.State.Features) != 0 || r.State.Returns["ready"] != 0 {
		t.Errorf("features %v, returns %v; a plan Odin did not keep is no plan, and no planner's to fix", r.State.Features, r.State.Returns)
	}
}

func TestATaskMayWorkInALeafTheSpecStageNamedAndInitHasNotCreatedYet(t *testing.T) {
	leaves := "leaves:\n  repos/api: { realm: helheim-go, realm_version: 0.1.0, description: The API. }\n"
	plan := planOf(taskOf("p-T001", "repos/api", "S"))
	script := planScript(plan) + "  leaves-file:\n    - actions: [ { write: { path: leaves.yaml, content: " + quote(leaves) + " } } ]\n      result: { status: ok, summary: wrote leaves.yaml, output: '{\"summary\": \"done\"}' }\n"
	nodes := planNodes + "  - { id: leaves-file, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n  - { id: ready, kind: tasks, from: plan, leaves: repos/app/leaves.yaml }\n"
	r := start(t, "key-points", "", nodes, script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	// With no leaves file, the layer is not a leaf of the project.
	noFile := strings.Replace(nodes, "leaves: repos/app/leaves.yaml", "leaves: repos/app/missing.yaml", 1)
	r = start(t, "key-points", "", noFile, script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "the leaves file repos/app/missing.yaml can't be read") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
	// A leaves file that doesn't parse is the spec stage's to fix.
	r = start(t, "key-points", "", nodes, strings.Replace(script, "realm: helheim-go, ", "", 1))
	out, err = Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "leaves.yaml") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}

// promptOf returns the prompt the scripted session named stem (node-attempt) got, as the agent read it.
func promptOf(t *testing.T, r *run.Run, stem string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", stem+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr struct{ Prompt string }
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Prompt
}

func TestARejectionGivesARefusalItsReturnsAgain(t *testing.T) {
	bad := planOf(taskOf("p-T001", "repos/app", "L"))
	good := planOf(taskOf("p-T001", "repos/app", "S"))
	nodes := planNodes + "  - { id: ready, kind: tasks, from: plan, back_to: plan, returns: 1 }\n  - { id: plan-gate, kind: gate, gate: plan }\n"
	r := start(t, "key-points", "gates:\n  plan: stop\n", nodes, planScript(bad, good, bad, good))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "plan-gate" || r.State.Returns["ready"] != 1 {
		t.Fatalf("Advance() = %+v, %v (returns %v)", out, err, r.State.Returns)
	}
	if err := Reject(context.Background(), r, "plan-gate", Reason("the plan is thin")); err != nil {
		t.Fatal(err)
	}
	if r.State.Returns["ready"] != 0 {
		t.Errorf("returns = %v after a rejection; a refusal may go back again", r.State.Returns)
	}
	out, err = Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "plan-gate" || r.State.Attempts["plan"] != 4 {
		t.Errorf("Advance() = %+v, %v (planner ran %d times); want the plan back at the gate after one more return", out, err, r.State.Attempts["plan"])
	}
}

func TestAPlanInAProjectWithNoLeafIsCheckedAgainstNoLeavesNotNotAtAll(t *testing.T) {
	r := startRealms(t, realmsSetup{nodes: planScratch + "  - { id: ready, kind: tasks, from: plan }\n", script: withPlan(planOf(taskOf("p-T001", "repos/anything", "S")), "steps: {}\n"),
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web.")}})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), `layer "repos/anything" is not a leaf of this project ()`) {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}
