package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/routing"
	"github.com/ouroboric-dev/odin/internal/run"
)

// contractTaskShape is the task shape with a pointer-contract gate before each task's pointer move.
var contractTaskShape = strings.Replace(taskShape, "      - { id: move, kind: pointer, realm: leaf }\n",
	"      - { id: contract-gate, kind: gate, gate: pointer-contract }\n      - { id: move, kind: pointer, realm: leaf }\n", 1)

// contractTaskRun is a plan of two tasks that depend on nothing: p-T1 changes the api leaf's contract, which
// the web leaf reads, and p-T2 builds web.txt.
func contractTaskRun(t *testing.T, mode, nodes string) *run.Run {
	t.Helper()
	noOasdiff(t)
	script := "steps:\n  p-t1-implement:\n    - actions:\n" +
		"        - write: { path: api.txt, content: \"done\\n\" }\n" +
		"        - write: { path: api.openapi.yaml, content: \"openapi: 3.0.3\\ninfo: { title: T, version: 2.0.0 }\\n\" }\n" +
		"      result: { status: ok, summary: wrote the contract, output: '{\"summary\": \"done\"}' }\n" + writeStep("p-t2-implement", "web.txt")
	r := startRealms(t, realmsSetup{mode: mode, nodes: nodes, script: script,
		realms: []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web implementer prompt."), recipeRealm("helheim-go", "0.2.0", "api.txt", "API implementer prompt.")},
		leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"}})
	plan := "# Tasks\n\n" + taskMD("p-T1", "repos/api", "S", "nothing") + taskMD("p-T2", "repos/web", "S", "nothing")
	if err := os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	imp := &routing.Impact{Schema: routing.ImpactSchema, Bound: "lower", Contracts: []routing.ContractEntry{
		{ID: "contract:repos/api/api.openapi.yaml", Path: "repos/api/api.openapi.yaml", Owner: "repos/api",
			Readers: routing.Reached{Extracted: []string{"web"}, Inferred: []string{}}}},
		Reach: map[string]routing.Reached{}, InferredEdges: []routing.Edge{}}
	if err := routing.WriteImpact(r.Project, imp); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tasks.md", routing.ImpactFile}, {"commit", "-q", "-m", "plan and impact index"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestAnUnattendedRunBlocksTheTaskWhoseMoveChangesAContractAndGoesOn(t *testing.T) {
	r := contractTaskRun(t, "unattended", contractTaskShape)
	before, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/api")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want the run done, with the task blocked", out, err)
	}
	status := map[string]schema.TaskStatus{}
	for _, f := range r.State.Features {
		status[f.ID] = f.Status
	}
	if status["p-T1"] != schema.TaskBlocked || status["p-T2"] != schema.TaskDone {
		t.Errorf("task statuses = %v, want p-T1 blocked and p-T2 done", status)
	}
	why := r.State.Features[0].Why
	for _, want := range []string{"changes a contract", "not in the approved spec", "repos/api/api.openapi.yaml", "gates/p-t1-contract-gate-1.md"} {
		if !strings.Contains(why, want) {
			t.Errorf("task 1's reason lacks %q: %q", want, why)
		}
	}
	if got := strings.Join(taskEvents(t, r), ", "); got != "task_started p-t1, task_blocked p-t1, task_started p-t2, task_done p-t2" {
		t.Errorf("task events = %s", got)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"pointer_contract_checked"`, `"ygg.verdict":"fail"`, `"ygg.result":"done, with 1 task(s) blocked: p-T1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"event":"gate_pending"`) {
		t.Errorf("the run stopped at the gate:\n%s", text)
	}
	if r.State.Pending != "" || r.State.PendingGate != "" {
		t.Errorf("the run is still pending at %s (%s)", r.State.Pending, r.State.PendingGate)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "gates", "p-t1-contract-gate-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Decision: blocked the task", "## Contract evidence", "repos/api/api.openapi.yaml"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, b)
		}
	}
	for _, bad := range []string{"ygg approve", "ygg reject", "stopped for a decision"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the gate file has %q, and nobody is to decide it:\n%s", bad, b)
		}
	}
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/api"); got != before {
		t.Error("the api pointer moved though its task was blocked")
	}
	api, _ := r.Leaf("repos/api")
	if main := mainOfLeaf(t, r, "repos/api"); r.State.Leaves["repos/api"].Checkpoint != main {
		t.Errorf("repos/api main is %s, checkpoint %s: the blocked task's merge is still on main", main, r.State.Leaves["repos/api"].Checkpoint)
	}
	if _, err := api.Git(bg, "show", "main:api.openapi.yaml"); err == nil {
		t.Error("the blocked task's contract is still on the leaf's main")
	}
}

func TestAContractGateOutsideATaskStillStopsUnattended(t *testing.T) {
	// the gate sits in the one-off shape of contract_test.go: no task, so nothing to block
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWriting("api.openapi.yaml"))
	noOasdiff(t)
	shipIndex(t, r, routing.Reached{Extracted: []string{"web"}})
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the gate", out, err)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "contract-gate-1.md"))
	if !strings.Contains(string(b), "ygg approve") || !strings.Contains(string(b), "stopped for a decision") {
		t.Errorf("the gate file doesn't ask for a decision:\n%s", b)
	}
}

func TestAContractGateInATaskStopsInKeyPoints(t *testing.T) {
	r := contractTaskRun(t, "key-points", contractTaskShape)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "p-t1-contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the gate", out, err)
	}
	if r.State.Features[0].Status == schema.TaskBlocked {
		t.Error("a task was blocked with an operator there to decide")
	}
}

func mainOfLeaf(t *testing.T, r *run.Run, repo string) string {
	t.Helper()
	l, err := r.Leaf(repo)
	if err != nil {
		t.Fatal(err)
	}
	main, err := l.Rev(bg, "main")
	if err != nil {
		t.Fatal(err)
	}
	return main
}
