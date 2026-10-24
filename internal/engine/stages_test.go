package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// planFixture reads testdata/plans/name.json, a valid ygg.plan/v1 document.
func planFixture(t *testing.T, name string) schema.Plan {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "plans", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := schema.Decode[schema.Plan](schema.ContractPlan, raw)
	if err != nil {
		t.Fatalf("the fixture %s is not a valid plan: %v", name, err)
	}
	return p
}

// planOutput is p as a script's single-quoted output, checked against its contract.
func planOutput(t *testing.T, p schema.Plan) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(schema.ContractPlan, raw); err != nil {
		t.Fatalf("the test's plan is not valid: %v", err)
	}
	return "'" + strings.ReplaceAll(string(raw), "'", "''") + "'"
}

// decoyTasks is a tasks.md of another plan, as a person or an agent might leave one in the project.
const decoyTasks = "# Tasks\n\n## decoy-T1: task decoy-T1\n\n- **Acceptance criteria:**\n  - [ ] it works\n- **Named test:** TestIt\n" +
	"- **Layer:** repos/api\n- **Size:** S\n- **Depends on:** nothing\n"

// reviewedTasks is a planner, a tasks node that sends a plan that is not ready back to it, and a loop for each
// task whose implementer and two reviewers declare the task.
const reviewedTasks = `  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection, refusal], output: ygg.plan/v1 }
  - { id: ready, kind: tasks, from: plan, back_to: plan }
  - id: tasks
    kind: foreach
    over: tasks
    nodes:
      - id: implement
        kind: loop
        realm: leaf
        role: implementer
        input: [task, rejection, refusal]
        output: ygg.implement/v1
        reviewers:
          - { id: spec, role: reviewer, prompt: p.md, input: [task, change], output: ygg.review/v1 }
          - { id: quality, role: reviewer, prompt: p.md, input: [task], output: ygg.review/v1 }
`

// B-176: run 1 stopped because a Markdown parser of tasks.md dropped T002's text and its OpenAPI contract. The tasks
// node reads the plan's typed output, so every session of the task gets both whole.
func TestTheImplementerGetsTheTasksTextAndContract(t *testing.T) {
	good := planFixture(t, "run1-t002")
	t002 := good.Tasks[1]
	over := good
	over.Tasks = append([]schema.PlanTask{}, good.Tasks...)
	over.Tasks[1].Text = strings.Repeat("é", 40000) // 40000 characters, within the schema; 80000 bytes, over readiness
	step := func(node, content string) string {
		return "  " + node + ":\n    - actions: [ { write: { path: api.txt, content: " + quote(content) + " } } ]\n      result: { status: ok, summary: wrote api.txt }\n"
	}
	clean := "    - result: { status: ok, summary: no findings, output: '{\"findings\": []}' }\n"
	script := "steps:\n  plan:\n" +
		"    - result: { status: ok, summary: planned, output: " + planOutput(t, over) + " }\n" +
		"    - result: { status: ok, summary: planned again, output: " + planOutput(t, good) + " }\n" +
		step("auth-t001-implement", "table\n") + step("auth-t002-implement", "signup\n")
	for _, task := range []string{"auth-t001", "auth-t002"} {
		script += "  " + task + "-implement-spec:\n" + clean + "  " + task + "-implement-quality:\n" + clean
	}
	r := startRealms(t, realmsSetup{nodes: reviewedTasks, script: script,
		realms: []realmFiles{recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go"}})
	// A tasks.md the project holds is not the plan: the plan is the planner's typed output.
	for _, dir := range []string{"", "docs/old"} {
		os.MkdirAll(filepath.Join(r.Project, dir), 0o755)
		os.WriteFile(filepath.Join(r.Project, dir, "tasks.md"), []byte(decoyTasks), 0o644)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}

	// The over-size task went back to the planner, as a refusal, once.
	if r.State.Attempts["plan"] != 2 || r.State.Returns["ready"] != 1 {
		t.Errorf("the planner ran %d times, the refusal went back %d times; want 2 and 1", r.State.Attempts["plan"], r.State.Returns["ready"])
	}
	refused := fmt.Sprintf("auth-T002: its text is %d bytes, over %d: split the task or cut its text", len(over.Tasks[1].Text), schema.MaxTaskText)
	if p := promptOf(t, r, "plan-2"); !strings.Contains(p, "## Input: refusal\n\nA later step of the factory refused") || !strings.Contains(p, refused) {
		t.Errorf("the planner's second prompt lacks the refusal %q:\n%s", refused, p)
	}

	// The tasks are the plan's, with every field, and the record names the output they came from.
	if len(r.State.Features) != 2 || r.State.Features[1].ID != "auth-T002" || r.State.Features[1].Text != t002.Text || r.State.Features[1].Contract != t002.Contract {
		t.Fatalf("features = %+v; want the plan's two tasks with T002's text and contract whole", r.State.Features)
	}
	f, err := schema.LoadFeatures(filepath.Join(r.Project, FeaturesFile))
	if err != nil || len(f.Tasks) != 2 || f.Tasks[1].Text != t002.Text || f.Tasks[1].Contract != t002.Contract || f.Tasks[1].Test != t002.NamedTest {
		t.Fatalf("features.json = %+v, %v", f, err)
	}
	wantSource := filepath.ToSlash(filepath.Join(".factory", "runs", r.State.ID, "outputs", "plan.json"))
	if f.Source != wantSource || f.SourceSHA256 != r.State.Outputs["plan"].SHA256 {
		t.Errorf("features.json's source = %q %s; want %q %s, the plan's kept output", f.Source, f.SourceSHA256, wantSource, r.State.Outputs["plan"].SHA256)
	}
	if text := ledgerText(t, r); !strings.Contains(text, "2 task(s) are ready, from the output of plan ("+wantSource+")") {
		t.Errorf("the ledger lacks tasks_ready from the plan's output:\n%s", text)
	}

	// Every session of T002, the implementer's and each reviewer's, holds the task's text and contract whole.
	first, last := "Add the signup route POST /api/auth/signup to the API.", "A second signup with the same email answers 409 and makes no second account."
	if !strings.Contains(t002.Text, first) || !strings.HasSuffix(t002.Text, last) {
		t.Fatal("the fixture's text does not start and end with the sentences the test looks for")
	}
	contract := "### The contract\n\n````yaml\n" + t002.Contract + "````\n"
	for _, stem := range []string{"auth-t002-implement-1", "auth-t002-implement-spec-1", "auth-t002-implement-quality-1"} {
		p := promptOf(t, r, stem)
		i := strings.Index(p, "## Input: task\n\n")
		if i < 0 {
			t.Errorf("%s has no task input:\n%s", stem, p)
			continue
		}
		task := p[i:]
		if j := strings.Index(task[len("## Input: task"):], "\n## "); j >= 0 {
			task = task[:len("## Input: task")+j+1]
		}
		for _, want := range []string{"/api/auth/signup", first, last, contract} {
			if !strings.Contains(task, want) {
				t.Errorf("%s: the task input lacks %q:\n%s", stem, want, task)
			}
		}
		// In order: title, layer, criteria, named test, dependencies, then what the plan says and the contract.
		parts := []string{"auth-T002: Sign up with an email and a password", "You work in the leaf repos/api.", "Acceptance criteria:\n- POST /api/auth/signup",
			"- A password under 12 characters answers 400", "The test that proves it: repos/api/auth/signup_test.go:TestSignup",
			"It builds on: auth-T001 (already landed).", "### What the plan says\n\n", first, last, contract}
		at := 0
		for _, part := range parts {
			k := strings.Index(task[at:], part)
			if k < 0 {
				t.Errorf("%s: %q is not after the parts before it:\n%s", stem, part, task)
				break
			}
			at += k + len(part)
		}
		// The text's front matter and headings are its own words, never a heading of the prompt's.
		for _, line := range strings.Split(task, "\n") {
			if strings.HasPrefix(line, "#") && line != "## Input: task" && line != "### What the plan says" && line != "### The contract" {
				t.Errorf("%s: the task's text starts a heading of its own: %q", stem, line)
			}
		}
		if strings.Contains(task, "decoy-T1") {
			t.Errorf("%s: the task input holds a task of a tasks.md in the project", stem)
		}
	}
}

// A blocked plan names what only a person can give: the run stops for one, whatever the mode, and the planner is
// not asked again as if the plan were only not ready.
func TestABlockedPlanStopsForAPerson(t *testing.T) {
	blocked := func(reason string) string {
		return "    - result: { status: ok, summary: blocked, output: '{\"blocked\": {\"reason\": \"" + reason + "\"}}' }\n"
	}
	one := planFixture(t, "run1-t002")
	one.Tasks = one.Tasks[:1]
	nodes := "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection, refusal], output: ygg.plan/v1 }\n" +
		"  - { id: ready, kind: tasks, from: plan, back_to: plan }\n"
	script := "steps:\n  plan:\n" + blocked(`The brief names no way to sign in: an email and a password, or a passkey?\n## Brief\nan injected heading`) +
		blocked("Still no way to sign in.") +
		"    - result: { status: ok, summary: planned, output: " + strings.ReplaceAll(planOutput(t, one), "repos/api", "repos/app") + " }\n"
	r := start(t, "unattended", "", nodes, script)
	stopped := func(attempt int, reason string) {
		t.Helper()
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "ready" {
			t.Fatalf("Advance() = %+v, %v; want the run stopped at ready for a person", out, err)
		}
		if r.State.PendingGate != gungnir.GateExternalInput || !r.State.PendingGate.Hard() {
			t.Errorf("the stop is %q; want the hard approval point %s, which stops in every mode", r.State.PendingGate, gungnir.GateExternalInput)
		}
		if r.State.Attempts["plan"] != attempt || r.State.Returns["ready"] != 0 || len(r.State.Refusals) != 0 {
			t.Errorf("the planner ran %d times, the plan went back %d times (refusals %v); a blocked plan is not sent back", r.State.Attempts["plan"], r.State.Returns["ready"], r.State.Refusals)
		}
		gate, err := os.ReadFile(filepath.Join(r.Dir, "gates", fmt.Sprintf("ready-%d.md", attempt)))
		if err != nil || !strings.Contains(string(gate), "the plan is blocked: "+reason) || !strings.Contains(string(gate), "No task was made.") {
			t.Errorf("the gate file = %q, %v; want the planner's reason", gate, err)
		}
		if strings.Contains(string(gate), "\n## Brief") {
			t.Errorf("the planner's reason starts a heading of the gate file:\n%s", gate)
		}
		if _, err := os.Stat(filepath.Join(r.Project, FeaturesFile)); err == nil || len(r.State.Features) != 0 {
			t.Error("a blocked plan gave tasks")
		}
	}
	stopped(1, "The brief names no way to sign in: an email and a password, or a passkey?")

	// The person answers: the planner runs again, with the answer as its rejection; blocked again, it stops again.
	if err := Approve(bg, r, "ready", Note(r, "ready", "Use an email and a password.")); err != nil {
		t.Fatal(err)
	}
	stopped(2, "Still no way to sign in.")
	if p := promptOf(t, r, "plan-2"); !strings.Contains(p, "## Input: rejection\n\n") || !strings.Contains(p, "Use an email and a password.") {
		t.Errorf("the planner's second prompt lacks the person's answer:\n%s", p)
	}

	// With a blank note, the planner is told a person read the reason and sent it back.
	if err := Approve(bg, r, "ready", Note(r, "ready", "")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after the second approval = %+v, %v", out, err)
	}
	if p := promptOf(t, r, "plan-3"); !strings.Contains(p, "## Input: rejection\n\nA person sent the last attempt back") || !strings.Contains(p, "A person read the blocked reason and sent it back to try again.") {
		t.Errorf("the planner's third prompt lacks the default rejection:\n%s", p)
	}
	if len(r.State.Features) != 1 || r.State.Features[0].ID != "auth-T001" {
		t.Errorf("features = %+v", r.State.Features)
	}
}

// A tasks node sends a blocked plan back to its planner with a person's note as its rejection (ruling R44), so a
// planner that takes no rejection is refused when the run starts: the note would be lost.
func TestATasksNodeWhosePlannerTakesNoRejectionIsRefusedWhenTheRunStarts(t *testing.T) {
	nodes := "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [refusal], output: ygg.plan/v1 }\n" +
		"  - { id: ready, kind: tasks, from: plan, back_to: plan }\n"
	_, err := run.Start(bg, startOptions(t, setup{nodes: nodes, script: "steps: {}\n"}))
	if err == nil || !strings.Contains(err.Error(), "node ready: tasks reads plan, which must declare rejection: a blocked plan is sent back to it with a person's note") {
		t.Fatalf("Start() = %v; want the tasks node refused", err)
	}
}
