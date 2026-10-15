package leaf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

// api returns the api leaf of the standard fixture, on a fresh run branch.
func api(t *testing.T) *Leaf {
	t.Helper()
	root := treetest.Build(t, treetest.Standard())
	nodes, err := tree.Resolve(ctx, root, "projects/gallery/repos/api")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(nodes[2])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.StartBranch(ctx, "ygg/run1", "main"); err != nil {
		t.Fatal(err)
	}
	return l
}

func write(t *testing.T, l *Leaf, path, body string) {
	t.Helper()
	p := filepath.Join(l.Dir(), path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCommitsTheLeafsWork(t *testing.T) {
	l := api(t)
	base, _ := l.Head(ctx)
	write(t, l, "src/app.go", "package app\n")
	sha, committed, err := l.Checkpoint(ctx, "ygg: run1 node build attempt 1")
	if err != nil || !committed {
		t.Fatalf("Checkpoint() = %s, %v, %v", sha, committed, err)
	}
	if sha == base {
		t.Error("the checkpoint is the old head")
	}
	if b, _ := l.repo.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); b != "ygg/run1" {
		t.Errorf("the checkpoint is on %q, want the run branch", b)
	}
	if files, _ := l.repo.Run(ctx, "show", "--name-only", "--format=", sha); files != "src/app.go" {
		t.Errorf("the checkpoint holds %q", files)
	}
	again, committed, err := l.Checkpoint(ctx, "nothing new")
	if err != nil || committed || again != sha {
		t.Errorf("a checkpoint with no change = %s, %v, %v; want the same head, no commit", again, committed, err)
	}
}

func TestCheckpointRefusesANestedRepository(t *testing.T) {
	l := api(t)
	before, _ := l.Head(ctx)
	marker := filepath.Join(t.TempDir(), "pwned")
	// What an agent can do with file writes alone: a submodule entry with a command as its URL,
	// and the pieces of a git directory under it.
	write(t, l, ".gitmodules", "[submodule \"x\"]\n\tpath = vendor/x\n\turl = ext::sh -c touch% "+marker+"\n")
	write(t, l, "vendor/x/.git/HEAD", "ref: refs/heads/main\n")
	write(t, l, "vendor/x/.git/objects/.keep", "")
	write(t, l, "vendor/x/.git/refs/heads/.keep", "")
	_, _, err := l.Checkpoint(ctx, "a nested repo")
	var pe *PolicyError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "vendor/x") {
		t.Fatalf("Checkpoint() = %v, want a *PolicyError naming vendor/x", err)
	}
	if after, _ := l.Head(ctx); after != before {
		t.Error("the refused checkpoint committed")
	}
	if staged, _ := l.repo.Run(ctx, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("the refused checkpoint left files staged: %q", staged)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the submodule URL's command ran")
	}
}

func TestCheckpointRefusesAChangedGitFile(t *testing.T) {
	l := api(t)
	evil := t.TempDir()
	write(t, l, ".git", "gitdir: "+evil+"\n")
	_, _, err := l.Checkpoint(ctx, "after a .git swap")
	var pe *PolicyError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("Checkpoint() = %v, want a *PolicyError about .git", err)
	}
	entries, _ := os.ReadDir(evil)
	if len(entries) != 0 {
		t.Error("git wrote to the planted git dir")
	}
}

func TestResetGoesBackToTheCheckpoint(t *testing.T) {
	l := api(t)
	write(t, l, ".gitignore", "build/\n")
	write(t, l, "keep.txt", "kept\n")
	cp, _, err := l.Checkpoint(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	write(t, l, "keep.txt", "changed\n")
	write(t, l, "new.txt", "untracked\n")
	write(t, l, "build/out.bin", "ignored\n")
	write(t, l, ".git", "gitdir: /tmp/elsewhere\n")
	if _, err := l.repo.Run(ctx, "switch", "-q", "-c", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := l.Reset(ctx, "ygg/run1", cp); err != nil {
		t.Fatalf("Reset() = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.Dir(), "keep.txt")); string(b) != "kept\n" {
		t.Errorf("keep.txt = %q, want the checkpoint's", b)
	}
	for _, gone := range []string{"new.txt", "build/out.bin"} {
		if _, err := os.Stat(filepath.Join(l.Dir(), gone)); err == nil {
			t.Errorf("%s survived the reset", gone)
		}
	}
	if head, _ := l.Head(ctx); head != cp {
		t.Errorf("HEAD = %s, want the checkpoint %s", head, cp)
	}
	if b, _ := l.repo.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); b != "ygg/run1" {
		t.Errorf("after the reset the leaf is on %q, want the run branch", b)
	}
	if _, _, err := l.Checkpoint(ctx, "after the reset"); err != nil {
		t.Errorf("the reset didn't restore the leaf's .git: %v", err)
	}
}

func TestStartBranchRefusesALeafWithChanges(t *testing.T) {
	root := treetest.Build(t, treetest.Standard())
	nodes, err := tree.Resolve(ctx, root, "projects/gallery/repos/api")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(nodes[2])
	if err != nil {
		t.Fatal(err)
	}
	write(t, l, "README.md", "edited by hand\n")
	if _, err := l.StartBranch(ctx, "ygg/run1", "main"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("StartBranch() = %v, want a refusal: a run never discards work it didn't make", err)
	}
	if _, err := Open(nodes[1]); err == nil {
		t.Error("Open() accepted an umbrella as a leaf")
	}
	if out, _ := git.Run(ctx, nodes[1].Dir, "status", "--porcelain"); !strings.Contains(out, "repos/api") {
		t.Errorf("the hand edit is gone: %q", out)
	}
}
