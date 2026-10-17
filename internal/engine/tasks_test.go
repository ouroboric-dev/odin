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

// taskMD is one task of a tasks.md, ready unless a field is left out of the arguments.
func taskMD(id, layer, size, deps string) string {
	return "## " + id + ": task " + id + "\n\n- **Acceptance criteria:**\n  - [ ] it works\n- **Named test:** TestIt\n- **Layer:** " + layer +
		"\n- **Size:** " + size + "\n- **Depends on:** " + deps + "\n\n"
}

const readyPlan = "# Tasks\n\n"

// planScript writes tasks.md in the leaf, the way a planner's session would: one entry for each attempt.
func planScript(plans ...string) string {
	var b strings.Builder
	b.WriteString("steps:\n  plan:\n")
	for _, p := range plans {
		b.WriteString("    - actions: [ { write: { path: tasks.md, content: " + quote(p) + " } } ]\n      result: { status: ok, summary: wrote tasks.md }\n")
	}
	return b.String()
}

func quote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
	return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"`
}

const planNodes = "  - { id: plan, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"

func TestAReadyPlanBecomesFeaturesInTheOrderTheyAreBuilt(t *testing.T) {
	plan := readyPlan + taskMD("gallery-T2", "repos/app", "M", "gallery-T1") + taskMD("gallery-T1", "repos/app", "S", "nothing")
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: repos/app/tasks.md }\n", planScript(plan))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(r.State.Features) != 2 || r.State.Features[0].ID != "gallery-T1" || r.State.Features[1].ID != "gallery-T2" ||
		r.State.Features[0].Status != schema.TaskTodo || r.State.PlanSource != "repos/app/tasks.md" || len(r.State.PlanSHA256) != 64 {
		t.Fatalf("Features = %+v, source %q", r.State.Features, r.State.PlanSource)
	}
	f, err := schema.LoadFeatures(filepath.Join(r.Project, ".factory", "features.json"))
	if err != nil || f.Run != r.State.ID || len(f.Tasks) != 2 || f.Tasks[0].ID != "gallery-T1" || f.Tasks[1].DependsOn[0] != "gallery-T1" {
		t.Errorf("features.json = %+v, %v", f, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"tasks_ready"`) || !strings.Contains(text, "2 task(s) are ready, from repos/app/tasks.md") {
		t.Errorf("the ledger lacks tasks_ready:\n%s", text)
	}
}

func TestAPlanThatIsNotReadyFailsTheNodeWithEveryProblem(t *testing.T) {
	bad := readyPlan + taskMD("p-T1", "repos/app", "", "nothing")
	r := start(t, "key-points", "", planNodes+"  - { id: ready, kind: tasks, from: repos/app/tasks.md }\n", planScript(bad))
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "no size") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
}

func TestThePlanMustBeOneRegularFile(t *testing.T) {
	r := start(t, "key-points", "", "  - { id: ready, kind: tasks, from: plans/*/tasks.md }\n", "steps: {}\n")
	app := r.Project
	run1 := func() string {
		out, err := Advance(context.Background(), r)
		if err != nil || out.Status != run.StatusFailed {
			t.Fatalf("Advance() = %+v, %v", out, err)
		}
		return out.Err.Error()
	}
	if msg := run1(); !strings.Contains(msg, "there is no tasks file at plans/*/tasks.md") {
		t.Errorf("no file: %s", msg)
	}
	for _, d := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(app, "plans", d), 0o755)
		os.WriteFile(filepath.Join(app, "plans", d, "tasks.md"), []byte(readyPlan+taskMD("p-T1", "repos/app", "S", "nothing")), 0o644)
	}
	if msg := run1(); !strings.Contains(msg, "2 files match") {
		t.Errorf("two files: %s", msg)
	}
	os.RemoveAll(filepath.Join(app, "plans", "b"))
	os.MkdirAll(filepath.Join(app, "plans", "b"), 0o755)
	os.Symlink("../a/tasks.md", filepath.Join(app, "plans", "b", "tasks.md")) // a link is not a plan
	os.Remove(filepath.Join(app, "plans", "a", "tasks.md"))
	if msg := run1(); !strings.Contains(msg, "there is no tasks file") {
		t.Errorf("a symlink: %s", msg)
	}
}

func TestATaskMayWorkInALeafTheSpecStageNamedAndInitHasNotCreatedYet(t *testing.T) {
	leaves := "leaves:\n  repos/api: { realm: helheim-go, realm_version: 0.1.0, description: The API. }\n"
	plan := readyPlan + taskMD("p-T1", "repos/api", "S", "nothing")
	script := planScript(plan) + "  leaves-file:\n    - actions: [ { write: { path: leaves.yaml, content: " + quote(leaves) + " } } ]\n      result: { status: ok, summary: wrote leaves.yaml }\n"
	nodes := planNodes + "  - { id: leaves-file, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n  - { id: ready, kind: tasks, from: repos/app/tasks.md, leaves: repos/app/leaves.yaml }\n"
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
