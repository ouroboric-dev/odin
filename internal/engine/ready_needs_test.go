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

// readyNodes are software's order: the plan in a scratch workspace, the tasks node that reads the leaves file,
// init, and the plan gate.
const readyNodes = "  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection, refusal], output: ygg.plan/v1 }\n" +
	"  - { id: ready, kind: tasks, from: plan, leaves: docs/leaves.yaml, back_to: plan }\n" +
	initNode +
	"  - { id: plan-gate, kind: gate, gate: plan }\n"

const twoPlans = "steps:\n  plan:\n    - result: { status: ok, summary: planned, output: '" + planDone + "' }\n    - result: { status: ok, summary: planned again, output: '" + planDone + "' }\n"

// readyRun starts a run of readyNodes with the leaves file and a ready plan for repos/api in the project.
func readyRun(t *testing.T, leaves string) *run.Run {
	t.Helper()
	r := startRealms(t, realmsSetup{nodes: readyNodes, script: twoPlans, realms: needyRealms()})
	writeLeaves(t, r, leaves)
	if err := os.WriteFile(filepath.Join(r.Project, "tasks.md"), []byte(readyPlan+taskMD("p-T1", "repos/api", "S", "nothing")), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReadyStopsOnAnUnmetNeed(t *testing.T) {
	ctx := context.Background()
	r := readyRun(t, galleryLeaves)
	out, err := Advance(ctx, r)
	unmetIn(t, "a leaves file with no runtime", out, err, "s3", "helheim-go")
	if r.State.Returns["ready"] != 0 || r.State.Attempts["plan"] != 1 || r.State.Refusals["plan"] != "" {
		t.Errorf("returns %d, plan attempts %d; the plan can't fix the leaves file, so nothing goes back to the planner", r.State.Returns["ready"], r.State.Attempts["plan"])
	}
	if r.State.Attempts["leaves"] != 0 || r.State.Attempts["ready"] != 1 {
		t.Errorf("attempts %v; the ready node fails, before init runs", r.State.Attempts)
	}
	if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 0 {
		t.Errorf("the project lists %v", u.Paths())
	}
	if len(r.State.Features) != 0 {
		t.Errorf("features %+v were recorded for a plan whose leaves can't be built", r.State.Features)
	}
	if _, took := r.State.Realms["helheim-go"]; took {
		t.Errorf("the ready node recorded realms: %v", r.State.Realms)
	}
	if text := ledgerText(t, r); strings.Contains(text, `"event":"realm_loaded"`) {
		t.Errorf("the ready node logged a realm_loaded:\n%s", text)
	}

	// A leaves file with the runtime pin passes: init runs, and the run goes on past the plan gate.
	ok := readyRun(t, runtimeLine("niflheim-local", "0.1.0")+galleryLeaves)
	out, err = Advance(ctx, ok)
	if err != nil || out.Status != run.StatusDone || len(ok.State.Features) != 1 {
		t.Fatalf("Advance() = %+v, %v; features %+v", out, err, ok.State.Features)
	}
	if u, _ := schema.LoadUmbrella(ok.Project); len(u.Children) != 2 || u.Runtime != "niflheim-local" {
		t.Errorf("init did not run after a ready plan: %+v", u)
	}
}
