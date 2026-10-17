package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// The checks of these tests: unit passes when the agent wrote out.txt (the loop's ladder), late passes
// only once it also wrote fixed.txt (the merge gate's), and accept does so in the project, with the leaf
// at the pointer's new commit (the pointer's acceptance).
const gateChecks = "checks:\n  unit: [test, -f, out.txt]\n  late: [test, -f, fixed.txt]\n  accept: [test, -f, repos/app/fixed.txt]\n"

const loopOfBuild = "  - { id: build, kind: loop, repo: repos/app, role: planner, prompt: p.md, checks: [unit] }\n"

// fixScript has the implementer write out.txt in attempt 1 and fixed.txt in the attempts after it.
const fixScript = "steps:\n  build:\n" +
	"    - actions: [ { write: { path: out.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote out.txt }\n" +
	"    - actions: [ { write: { path: fixed.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote fixed.txt }\n" +
	"    - actions: [ { write: { path: more.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: wrote more.txt }\n"

func TestAMergeGateThatRefusesTheChecksSendsTheWorkBackIntoTheLoop(t *testing.T) {
	nodes := loopOfBuild + "  - { id: merge, kind: merge, repo: repos/app, checks: [late], back_to: build }\n"
	r := startPack(t, "key-points", "", gateChecks, nodes, fixScript)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Attempts["build"] != 2 || r.State.Returns["merge"] != 1 {
		t.Errorf("the implementer ran %d times, the refusal went back %d times; want 2 and 1", r.State.Attempts["build"], r.State.Returns["merge"])
	}
	prompt := promptOf(t, r, "build-2")
	for _, want := range []string{"## Refused after your work passed", "merge gate refused (checks)", "check late failed"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the second attempt's prompt lacks %q:\n%s", want, prompt)
		}
	}
	l, _ := r.Leaf("repos/app")
	if body, err := l.Git(bg, "show", "main:fixed.txt"); err != nil || body != "done" {
		t.Errorf("main:fixed.txt = %q, %v; the second attempt's work landed", body, err)
	}
	if rec := r.State.Leaves["repos/app"]; rec.Checkpoint == "" {
		t.Error("the leaf has no checkpoint")
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"node_returned"`) || !strings.Contains(text, "back to build, 1 of 2") {
		t.Errorf("the ledger lacks node_returned:\n%s", text)
	}
}

func TestARefusalThatKeepsComingBackFailsTheNodeAfterItsReturns(t *testing.T) {
	nodes := loopOfBuild + "  - { id: merge, kind: merge, repo: repos/app, checks: [late], back_to: build, returns: 1 }\n"
	script := strings.Replace(fixScript, "writes fixed.txt", "wrote nothing useful", 1)
	script = strings.ReplaceAll(script, "fixed.txt", "nothing.txt")
	r := startPack(t, "key-points", "", gateChecks, nodes, script)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "check late failed") {
		t.Fatalf("Advance() = %+v, %v; want the node failed with the gate's refusal", out, err)
	}
	if r.State.Attempts["build"] != 2 || r.State.Returns["merge"] != 1 {
		t.Errorf("implementer %d times, returned %d times; want 2 and 1", r.State.Attempts["build"], r.State.Returns["merge"])
	}
	l, _ := r.Leaf("repos/app")
	if _, err := l.Git(bg, "show", "main:out.txt"); err == nil {
		t.Error("work the gate refused landed on main")
	}
}

func TestABaseThatDoesNotMergeCleanlyIsNotSentBack(t *testing.T) {
	nodes := loopOfBuild + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit], back_to: build }\n"
	r := startPack(t, "key-points", "", gateChecks, nodes, fixScript)
	l, _ := r.Leaf("repos/app")
	// main gets an out.txt of its own, so the run's out.txt conflicts with it.
	for _, args := range [][]string{{"switch", "-q", "main"}, {"switch", "-q", run.Branch(r.State.ID)}} {
		if args[2] == "main" {
			if _, err := l.Git(bg, args...); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(l.Dir(), "out.txt"), []byte("someone else's\n"), 0o644)
			for _, step := range [][]string{{"add", "out.txt"}, {"commit", "-q", "-m", "someone else landed out.txt"}} {
				if _, err := l.Git(bg, step...); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		if _, err := l.Git(bg, args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "conflict") {
		t.Fatalf("Advance() = %+v, %v; want failed on the conflict", out, err)
	}
	if r.State.Returns["merge"] != 0 || r.State.Attempts["build"] != 1 {
		t.Errorf("returns %d, implementer ran %d times; a conflict is not the implementer's to fix", r.State.Returns["merge"], r.State.Attempts["build"])
	}
}

func TestAPointerMoveWhoseAcceptanceFailsSendsTheWorkBackIntoTheLoop(t *testing.T) {
	nodes := loopOfBuild + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, checks: [unit], acceptance: [accept], back_to: build }\n"
	r := startPack(t, "key-points", "", gateChecks, nodes, fixScript)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Attempts["build"] != 2 || r.State.Returns["move"] != 1 || r.State.Attempts["merge"] != 2 {
		t.Errorf("implementer %d times, merge %d times, returned %d times; want 2, 2 and 1", r.State.Attempts["build"], r.State.Attempts["merge"], r.State.Returns["move"])
	}
	if prompt := promptOf(t, r, "build-2"); !strings.Contains(prompt, "pointer move refused (acceptance)") {
		t.Errorf("the second attempt's prompt lacks the pointer's refusal:\n%s", prompt)
	}
	l, _ := r.Leaf("repos/app")
	main, _ := l.Rev(bg, "main")
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got != main {
		t.Errorf("the project points at %s, want main %s", got, main)
	}
}
