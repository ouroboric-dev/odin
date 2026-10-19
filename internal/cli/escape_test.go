package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// The escape-attempt tests of spec §13 that need no sandbox: a scripted agent tries to reach git
// or Odin's records from inside its leaf, and Odin blocks or ignores it. The sandbox's own tests
// (initializeCommand, ~/.ssh) come with Phase 3.

// escape starts a toy run in unattended mode, with design: auto so it never stops at a gate, whose
// design node is the given scripted step, and returns the project, the run's id and ygg's exit.
func escape(t *testing.T, designStep string) (dir, id string, code int) {
	t.Helper()
	toy, _ := filepath.Abs("../../testdata/toy/script.yaml")
	b, err := os.ReadFile(toy)
	if err != nil {
		t.Fatal(err)
	}
	// The toy script with design's step replaced.
	i := strings.Index(string(b), "  design:\n")
	j := strings.Index(string(b), "  spec:\n")
	script := filepath.Join(t.TempDir(), "script.yaml")
	if err := os.WriteFile(script, []byte(string(b[:i])+"  design:\n"+designStep+string(b[j:])), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, config := project(t, "script.yaml", "mode: unattended\ngates: { design: auto }\n")
	cfg, _ := os.ReadFile(config)
	if err := os.WriteFile(config, []byte(strings.ReplaceAll(string(cfg), toy, script)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ = ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	entries, _ := os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	if len(entries) == 1 {
		id = entries[0].Name()
	}
	return dir, id, code
}

// app returns the repos/app leaf of the project in dir, as the tree records it.
func app(t *testing.T, dir string) tree.Node {
	t.Helper()
	root, rel, err := tree.FindRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := tree.Resolve(context.Background(), root, filepath.Join(rel, "repos/app"))
	if err != nil {
		t.Fatal(err)
	}
	return nodes[len(nodes)-1]
}

func verifies(t *testing.T, dir, id string) {
	t.Helper()
	s, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Verify(filepath.Join(run.Dir(dir, id), "ledger.jsonl"), s.Ledger); err != nil {
		t.Errorf("the ledger doesn't verify: %v", err)
	}
}

const ok = "      result: { status: ok, summary: wrote design.md }\n"

func TestAnAgentCannotPlantAHookInTheLeaf(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hook-ran")
	dir, id, code := escape(t, "    - actions: [ { write: { path: .git/hooks/pre-commit, content: \"#!/bin/sh\\ntouch "+marker+"\\n\" } } ]\n"+ok)
	if code != ExitFailed {
		t.Errorf("exit %d, want %d: the leaf's .git is a file, so the write fails and the node with it", code, ExitFailed)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a planted hook ran")
	}
	verifies(t, dir, id)
}

func TestAHookInTheLeafsGitDirNeverRuns(t *testing.T) {
	// As if the agent could reach the git dir, which the sandbox mounts read-only (Phase 3): a
	// hook there still never runs, because host git runs with hooks off.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	dir, config := project(t, "script.yaml", "")
	code, _, _ := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	if code != ExitStopped {
		t.Fatalf("run: exit %d", code)
	}
	hook := filepath.Join(app(t, dir).GitDir, "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	id := runID(t, dir)
	if code, out, errOut := ygg(t, "approve", id, "design-gate", "--project", dir); code != ExitDone {
		t.Fatalf("approve: exit %d:\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the hook ran during a checkpoint commit")
	}
}

func TestARewrittenGitFileStopsTheRun(t *testing.T) {
	evil := t.TempDir()
	// Attempt 1 rewrites .git; attempt 2, after the approval, does the design as the toy does.
	dir, id, code := escape(t, "    - actions: [ { write: { path: .git, content: \"gitdir: "+evil+"\\n\" } } ]\n"+ok+
		"    - actions: [ { write: { path: design.md, content: \"# Design\\n\" } } ]\n"+ok)
	if code != ExitStopped {
		t.Fatalf("exit %d, want %d: a stop at policy-violation", code, ExitStopped)
	}
	s, _ := run.LoadState(run.Dir(dir, id))
	if s.PendingGate != gungnir.GatePolicyViolation {
		t.Errorf("the run waits at %q, want policy-violation", s.PendingGate)
	}
	if entries, _ := os.ReadDir(evil); len(entries) != 0 {
		t.Error("git wrote to the git dir the agent planted")
	}
	verifies(t, dir, id)
	// Approving the stop puts the leaf back to its checkpoint and runs the node again (decision 9).
	if code, out, errOut := ygg(t, "approve", id, "design", "--project", dir); code != ExitDone {
		t.Fatalf("approve: exit %d, want %d:\n%s%s", code, ExitDone, out, errOut)
	}
	if entries, _ := os.ReadDir(evil); len(entries) != 0 {
		t.Error("git wrote to the planted git dir during the approval")
	}
	verifies(t, dir, id)
}

func TestANestedSubmoduleIsNeverFetched(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	plant := "    - actions:\n" +
		"        - write: { path: .gitmodules, content: \"[submodule \\\"x\\\"]\\n\\tpath = vendor/x\\n\\turl = ext::sh -c touch% " + marker + "\\n\" }\n" +
		"        - write: { path: vendor/x/.git/HEAD, content: \"ref: refs/heads/main\\n\" }\n" +
		"        - write: { path: vendor/x/.git/objects/.keep, content: \"\" }\n" +
		"        - write: { path: vendor/x/.git/refs/heads/.keep, content: \"\" }\n" + ok
	dir, id, code := escape(t, plant)
	if code != ExitStopped {
		t.Fatalf("exit %d, want %d: a stop at policy-violation", code, ExitStopped)
	}
	leaf := app(t, dir)
	l := git.Repo{GitDir: leaf.GitDir, WorkTree: leaf.Dir}
	s, _ := run.LoadState(run.Dir(dir, id))
	if head, _ := l.Run(context.Background(), "rev-parse", "HEAD"); head != s.Leaves["repos/app"].Checkpoint {
		t.Error("Odin committed the planted submodule")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the submodule's URL ran a command")
	}
	verifies(t, dir, id)
}

func TestAnAgentCannotWriteOdinsRecords(t *testing.T) {
	// A path out of the leaf is refused when the script loads, before anything runs.
	dir, _, code := escape(t, "    - actions: [ { write: { path: ../../.factory/runs/x/ledger.jsonl, content: \"{}\\n\" } } ]\n"+ok)
	if code != ExitRefused {
		t.Errorf("a script that writes out of its leaf: exit %d, want %d", code, ExitRefused)
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory")); err == nil {
		t.Error("the refused run created .factory/")
	}
	// A file named like a record, inside the leaf, is only a file in the leaf: Odin's records are
	// in the project's .factory/, and the run's ledger still verifies.
	dir, id, code := escape(t, "    - actions: [ { write: { path: .factory/runs/x/ledger.jsonl, content: \"{}\\n\" } } ]\n"+ok)
	if code != ExitDone {
		t.Errorf("exit %d, want %d", code, ExitDone)
	}
	verifies(t, dir, id)
}
