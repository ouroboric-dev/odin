package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

var bg = context.Background()

// checks are the pack checks of these tests: unit passes when the agent wrote out.txt, and e2e runs
// in the project umbrella.
const checks = "checks:\n  unit: [test, -f, out.txt]\n  e2e: [test, -f, umbrella.yaml]\n  broken: [\"false\"]\n"

const build = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"

func TestAMergeNodeLandsTheBranchThroughTheGate(t *testing.T) {
	r := startPack(t, "key-points", "", checks, build+"  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n", okScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	l, _ := r.Leaf("repos/app")
	main, _ := l.Rev(bg, "main")
	head, _ := l.Head(bg)
	if main != head || r.State.Leaves["repos/app"].Checkpoint != main {
		t.Errorf("main %s, head %s, checkpoint %s; want main landed at the run's head", main, head, r.State.Leaves["repos/app"].Checkpoint)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"check_result","ygg.attempt":1,"ygg.result":"unit: exit 0"`,
		`"ygg.node.id":"merge","event":"node_finished"`, `"ygg.ref.commit":"` + main + `"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s", want)
		}
	}
}

func TestAMergeNodeFailsWhenTheChecksFail(t *testing.T) {
	r := startPack(t, "key-points", "", checks, build+"  - { id: merge, kind: merge, repo: repos/app, checks: [broken] }\n", okScript)
	base := r.State.Leaves["repos/app"].Base
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "check broken") {
		t.Fatalf("Advance() = %+v, %v; want failed on the broken check", out, err)
	}
	l, _ := r.Leaf("repos/app")
	if main, _ := l.Rev(bg, "main"); main != base {
		t.Error("main moved although the merge gate's checks failed")
	}
}

func TestAPointerNodeMovesTheProjectsPointer(t *testing.T) {
	nodes := build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, checks: [unit], acceptance: [e2e] }\n"
	r := startPack(t, "key-points", "", checks, nodes, okScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	l, _ := r.Leaf("repos/app")
	main, _ := l.Rev(bg, "main")
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got != main {
		t.Errorf("the project points at %s, want the leaf's main %s", got, main)
	}
	if !strings.Contains(ledgerText(t, r), `"event":"pointer_moved"`) {
		t.Error("the ledger doesn't record the pointer move")
	}
}
