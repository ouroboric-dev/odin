package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

const acceptanceStack = "docker.io/library/node:24.1.0-bookworm@sha256:3333333333333333333333333333333333333333333333333333333333333333"

// acceptancePack is the sandboxed pack with a stack for acceptance checks, and an acceptance check that
// looks for the build in the snapshot of the project, where the leaf is at repos/app.
func acceptancePack(accept string) string {
	p := strings.Replace(sandboxPack, "checks:\n", "checks:\n  accept: "+accept+"\n", 1)
	return p + "  acceptance: { stack: " + acceptanceStack + " }\n"
}

const acceptanceNodes = buildNodes + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
	"  - { id: move, kind: pointer, repo: repos/app, checks: [unit], acceptance: [accept] }\n"

func TestAcceptanceChecksRunInASandboxThatHoldsASnapshotOfTheProject(t *testing.T) {
	r, fake := startSandboxedPack(t, acceptancePack("[grep, -q, done, repos/app/build.txt]"), acceptanceNodes, buildScript, run.Hooks{})
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	var snap *gungnir.SandboxSpec
	for i, u := range fake.Ups {
		if u.Name == "snapshot/move/test" {
			snap = &fake.Ups[i]
		}
	}
	if snap == nil {
		t.Fatalf("no snapshot sandbox came up: %+v", fake.Ups)
	}
	if !snap.Scratch || snap.GitDir != "" || snap.Stack != acceptanceStack || snap.Leaf != filepath.Join(r.Dir, "snapshot", "move", "test") || snap.Parent != r.Dir {
		t.Errorf("spec = %+v; want a scratch sandbox on the acceptance stack over the run's snapshot folder", *snap)
	}
	if err := snap.Validate(); err != nil {
		t.Errorf("the spec Odin built is one the driver would refuse: %v", err)
	}
	var ran bool
	for _, c := range fake.Execs {
		ran = ran || (len(c.Argv) > 1 && c.Argv[0] == "grep" && c.Argv[3] == "repos/app/build.txt")
	}
	if !ran {
		t.Errorf("the acceptance check did not go through the sandbox: %+v", fake.Execs)
	}
	if b, err := os.ReadFile(filepath.Join(r.Dir, "snapshot", "move", "test", "repos", "app", "build.txt")); err != nil || string(b) != "done\n" {
		t.Errorf("the snapshot holds repos/app/build.txt = %q, %v; the leaf at the commit it moves to", b, err)
	}
	filepath.WalkDir(filepath.Join(r.Dir, "snapshot"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == ".git" {
			t.Errorf("the snapshot holds %s", p)
		}
		return nil
	})
	if len(fake.Downs) != len(fake.Ups) {
		t.Errorf("%d sandboxes up, %d down; the snapshot sandbox is stopped with the rest", len(fake.Ups), len(fake.Downs))
	}
	if !strings.Contains(ledgerText(t, r), `"event":"pointer_moved"`) {
		t.Error("the pointer did not move")
	}
}

func TestAFailingAcceptanceCheckInTheSnapshotRefusesThePointerMove(t *testing.T) {
	r, _ := startSandboxedPack(t, acceptancePack("[grep, -q, never-written, repos/app/build.txt]"), acceptanceNodes, buildScript, run.Hooks{})
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "check accept") {
		t.Fatalf("Advance() = %+v, %v; want the move refused on the acceptance check", out, err)
	}
	if strings.Contains(ledgerText(t, r), `"event":"pointer_moved"`) {
		t.Error("the pointer moved although acceptance failed")
	}
}

// What a leaf holds beyond its commit (ignored build output such as node_modules) never reaches the
// snapshot, so an acceptance check can't depend on it and the snapshot stays small.
func TestWhatALeafIgnoresIsNotInTheSnapshot(t *testing.T) {
	script := "steps:\n  a:\n    - actions:\n        - write: { path: build.txt, content: \"done\\n\" }\n        - write: { path: .gitignore, content: \"ignored.txt\\n\" }\n" +
		"        - write: { path: ignored.txt, content: \"build output\\n\" }\n      result: { status: ok, summary: built }\n"
	r, _ := startSandboxedPack(t, acceptancePack("[sh, -c, \"test -f repos/app/build.txt && test ! -e repos/app/ignored.txt\"]"), acceptanceNodes, script, run.Hooks{})
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "ignored.txt")); err != nil {
		t.Errorf("the leaf itself lost its ignored file: %v", err)
	}
}
