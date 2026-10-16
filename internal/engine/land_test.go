package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
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

func TestAShrunkGeneratedFileStopsAtTheShrinkGuard(t *testing.T) {
	nodes := build + "  - { id: index, kind: exec, run: [sh, -c, \"echo '| one |' > REPOS.md\"], repo: . }\n" +
		"  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, generated: [REPOS.md] }\n"
	r := startPack(t, "unattended", "", checks, nodes, okScript)
	if err := os.WriteFile(filepath.Join(r.Project, "REPOS.md"), []byte(strings.Repeat("| row |\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "REPOS.md"}, {"commit", "-q", "-m", "REPOS.md"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "move" || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v (pending %s); want a stop at shrink-guard, even in unattended mode", out, err, r.State.PendingGate)
	}
	if b, err := os.ReadFile(filepath.Join(r.Dir, "gates", "move-1.md")); err != nil || !strings.Contains(string(b), "keeps 1 of its 10 rows") {
		t.Errorf("gate file = %q, %v", b, err)
	}
	if err := Reject(bg, r, "move", "no"); err == nil {
		t.Error("Reject() of a hard stop succeeded; a hard stop takes approve, or a new run")
	}
	if err := Approve(bg, r, "move", "the index shrank on purpose"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after approval = %+v, %v", out, err)
	}
	if files, _ := git.Run(bg, r.Project, "show", "--name-only", "--format=", "HEAD"); files != "REPOS.md\nrepos/app" {
		t.Errorf("the approved move commits %q, want the shrunk REPOS.md with the pointer", files)
	}
}

func TestAPlantedRepositoryStopsAtPolicyViolation(t *testing.T) {
	script := "steps:\n  a:\n    - actions:\n        - write: { path: vendor/x/.git/HEAD, content: \"ref: refs/heads/main\\n\" }\n" +
		"        - write: { path: vendor/x/.git/objects/.keep, content: \"\" }\n        - write: { path: vendor/x/.git/refs/heads/.keep, content: \"\" }\n" +
		"      result: { status: ok, summary: planted a repo }\n" +
		"    - actions: [ { write: { path: out.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: clean }\n"
	r := startPack(t, "unattended", "", "", build, script)
	base := r.State.Leaves["repos/app"].Checkpoint
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v; want a stop at policy-violation, even in unattended mode", out, err)
	}
	l, _ := r.Leaf("repos/app")
	if head, _ := l.Head(bg); head != base {
		t.Error("Odin committed the planted repository")
	}
	planted := filepath.Join(r.RepoDir("repos/app"), "vendor", "x", ".git", "HEAD")
	id, project := r.State.ID, r.Project
	r.Close()
	reopened, err := run.Open(bg, project, id, "")
	if err != nil {
		t.Fatalf("Open() of a quarantined run = %v", err)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Error("Open() cleaned the quarantined leaf before anyone decided; the evidence is gone")
	}
	if err := Approve(bg, reopened, "a", "investigated"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Error("approving the stop didn't put the leaf back to its checkpoint")
	}
	if out, err := Advance(bg, reopened); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after approval = %+v, %v; want the node re-run from its checkpoint", out, err)
	}
	reopened.Close()
}

// Review focus 1: leaf code, here a pointer node's check, rewrites the leaf's .git file. Host git
// in the parent never reads that file, and the rewrite stops the run at policy-violation before
// anything lands; the pointer stays where it was.
func TestALeafCheckThatRewritesGitStopsTheRun(t *testing.T) {
	nodes := build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, checks: [rewrite] }\n"
	r := startPack(t, "unattended", "", checks+"  rewrite: [sh, -c, \"echo 'gitdir: /nonexistent' > .git\"]\n", nodes, okScript)
	before, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Errorf("Advance() = %+v, %v (pending %s); want a stop at policy-violation", out, err, r.State.PendingGate)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); after != before {
		t.Error("the pointer moved although a check rewrote the leaf's .git")
	}
}
