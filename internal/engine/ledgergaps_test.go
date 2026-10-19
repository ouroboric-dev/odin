package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// shrunkRun starts a run whose pointer move meets an INDEX.md that kept 1 of its 10 rows; the move's
// acceptance check passes while ok.flag is in the project.
func shrunkRun(t *testing.T) *run.Run {
	t.Helper()
	nodes := build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, acceptance: [flag], generated: [INDEX.md] }\n"
	r := startPack(t, "unattended", "", packChecks+"  flag: [test, -f, ok.flag]\n", nodes, okScript)
	if err := os.WriteFile(filepath.Join(r.Project, "ok.flag"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Project, "INDEX.md"), []byte(strings.Repeat("| row |\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "INDEX.md"}, {"commit", "-q", "-m", "INDEX.md"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.Project, "INDEX.md"), []byte("| one |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

// nodeEvents lists the events of one node in the ledger as "event attempt".
func nodeEvents(t *testing.T, r *run.Run, node string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(ledgerText(t, r)), "\n") {
		var e struct {
			Event   string `json:"event"`
			Node    string `json:"ygg.node.id"`
			Attempt int    `json:"ygg.attempt"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Node == node {
			out = append(out, e.Event+" "+string(rune('0'+e.Attempt)))
		}
	}
	return out
}

func TestAShrinkGuardStopClosesTheAttemptTheLedgerOpened(t *testing.T) {
	r := shrunkRun(t)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := strings.Join(nodeEvents(t, r, "move"), ", "); got != "node_started 1, check_result 1, node_failed 1, gate_pending 1" {
		t.Errorf("the move's ledger lines = %s; the attempt that started must end", got)
	}
}

func TestAWaiverIsForOneMoveWhateverBecomesOfIt(t *testing.T) {
	r := shrunkRun(t)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Approve(bg, r, "move", "the index shrank on purpose"); err != nil {
		t.Fatal(err)
	}
	if !r.State.Waived["move"] {
		t.Fatal("approving the shrink-guard stop did not waive the next move")
	}
	os.Remove(filepath.Join(r.Project, "ok.flag")) // the move now fails its acceptance check, after the guard is skipped
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want the move refused by its acceptance check", out, err)
	}
	if r.State.Waived["move"] {
		t.Error("the waiver outlived the failed move")
	}
	os.WriteFile(filepath.Join(r.Project, "ok.flag"), nil, 0o644)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Errorf("Advance() = %+v, %v (pending %s); the next move meets the guard again, since the waiver was for one move", out, err, r.State.PendingGate)
	}
}

func TestAMoveThatCommittedBeforeTheRunRecordedItIsLoggedOnTheNextAttempt(t *testing.T) {
	nodes := build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: pointer-gate, kind: gate, gate: pointer }\n  - { id: move, kind: pointer, repo: repos/app }\n"
	r := startPack(t, "key-points", "gates:\n  pointer: stop\n", packChecks, nodes, okScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || out.Gate != "pointer-gate" {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	// As if an earlier attempt of the move committed, and the process died before the ledger got the line.
	l, _ := r.Leaf("repos/app")
	main, _ := l.Rev(bg, "main")
	for _, args := range [][]string{{"update-index", "--cacheinfo", "160000," + main + ",repos/app"}, {"commit", "-q", "-m", "the move an earlier attempt made"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	r.State.Attempts["move"] = 1
	if err := Approve(bg, r, "pointer-gate", ""); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"pointer_moved"`) || !strings.Contains(text, "an earlier attempt moved it and the run didn't record it") {
		t.Errorf("the ledger lacks the move the earlier attempt made:\n%s", text)
	}
	// A move that was already there on the node's first attempt was not this node's: nothing is logged.
}
