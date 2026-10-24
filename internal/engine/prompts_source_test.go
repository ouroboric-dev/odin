package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

// The prompts of agent nodes and of a loop's reviewers are the graph pack's; the implementer of a task's
// loop is the realm of the task's leaf. The leaf's realm holds a reviewer prompt of its own, which no
// session may read.
func TestPromptsComeFromTheGraphPackAndTheImplementerFromTheLeafsRealm(t *testing.T) {
	graph := realmFiles{name: "graph-toy", version: "0.1.0", files: map[string]string{
		"prompts/design.md": "GRAPH-PACK design.\n", "prompts/reviewer-spec.md": "GRAPH-PACK reviewer.\n", "p.md": "GRAPH-PACK plan.\n"}}
	leafRealm := recipeRealm("leaf-toy", "0.2.0", "api.txt", "LEAF-REALM implementer.")
	leafRealm.files["prompts/reviewer-spec.md"] = "WRONG\n"
	leafRealm.files["prompts/design.md"] = "WRONG\n"
	nodes := "  - { id: design, kind: agent, role: planner, prompt: prompts/design.md, repo: repos/api, input: [rejection], output: ygg.implement/v1 }\n" +
		strings.Replace(taskShape, "      - { id: implement, kind: loop, realm: leaf, role: implementer, input: [rejection, refusal], output: ygg.implement/v1 }\n",
			"      - { id: implement, kind: loop, realm: leaf, role: implementer, reviewers: [ { id: spec, role: reviewer, prompt: prompts/reviewer-spec.md, output: ygg.review/v1 } ], input: [rejection, refusal], output: ygg.implement/v1 }\n", 1)
	script := "steps:\n  design:\n    - result: { status: ok, summary: designed, output: '{\"summary\": \"done\"}' }\n" + writeStep("p-t1-implement", "api.txt") +
		"  p-t1-implement-spec:\n    - result: { status: ok, summary: no findings, report: [] }\n"
	r := startRealms(t, realmsSetup{nodes: nodes, script: script, realms: []realmFiles{graph, leafRealm}, leaves: map[string]string{"repos/api": "leaf-toy"}})
	if _, ok := r.State.Realms["graph-toy"]; ok {
		t.Errorf("run.json realms = %v; a graph pack with no recipe is no realm of the run", r.State.Realms)
	}
	plan := "# Tasks\n\n" + taskMD("p-T1", "repos/api", "S", "nothing")
	if err := os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	for _, stem := range []string{"design-1", "p-t1-implement-spec-1"} {
		if got := promptOf(t, r, stem); !strings.Contains(got, "GRAPH-PACK") || strings.Contains(got, "WRONG") {
			t.Errorf("the prompt of %s is from the wrong pack:\n%s", stem, got)
		}
	}
	if got := promptOf(t, r, "p-t1-implement-1"); !strings.Contains(got, "LEAF-REALM") || strings.Contains(got, "GRAPH-PACK") {
		t.Errorf("the implementer's prompt is from the wrong pack:\n%s", got)
	}
}
