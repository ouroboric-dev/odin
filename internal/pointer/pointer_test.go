package pointer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/shrink"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

func ok(context.Context) error { return nil }

// landed returns the instance root, the gallery umbrella, and a spec moving its repos/api pointer
// to a new commit on the leaf's main.
func landed(t *testing.T) (string, tree.Node, Spec) {
	t.Helper()
	root := treetest.Build(t, treetest.Standard())
	nodes, err := tree.Resolve(ctx, root, "projects/gallery/repos/api")
	if err != nil {
		t.Fatal(err)
	}
	l, err := leaf.Open(nodes[2])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.StartBranch(ctx, "ygg/run1", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Dir(), "feature.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, _, err := l.Checkpoint(ctx, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Git(ctx, "update-ref", "refs/heads/main", head); err != nil { // as the merge gate lands it
		t.Fatal(err)
	}
	return root, nodes[1], Spec{Parent: nodes[1], Child: "repos/api", Leaf: l, To: head,
		Lock: filepath.Join(nodes[1].Dir, ".factory", "locks", "project.lock"), Check: ok, Acceptance: ok,
		Message: "Move repos/api (run run1)"}
}

func gitlink(t *testing.T, dir, path string) string {
	t.Helper()
	sha, err := git.Run(ctx, dir, "rev-parse", "HEAD:"+path)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func count(t *testing.T, dir string) string {
	t.Helper()
	n, err := git.Run(ctx, dir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMoveCommitsOneLevelOnly(t *testing.T) {
	root, parent, spec := landed(t)
	rootCommits := count(t, root)
	res, err := Move(ctx, spec)
	if err != nil {
		t.Fatalf("Move() = %v", err)
	}
	if got := gitlink(t, parent.Dir, "repos/api"); got != spec.To || res.To != spec.To || res.Commit == "" {
		t.Errorf("the gallery points at %s (result %+v), want %s", got, res, spec.To)
	}
	if msg, _ := git.Run(ctx, parent.Dir, "log", "-1", "--format=%s"); msg != spec.Message {
		t.Errorf("commit message = %q", msg)
	}
	if count(t, root) != rootCommits {
		t.Error("the move committed in the root too; a move spans one level (spec §3.4)")
	}
	if again, err := Move(ctx, spec); err != nil || again.Commit != "" {
		t.Errorf("a second Move() to the same commit = %+v, %v; want no commit", again, err)
	}
}

func TestMoveRunsTheChecksAndTheAcceptanceTests(t *testing.T) {
	_, parent, spec := landed(t)
	before := gitlink(t, parent.Dir, "repos/api")
	for name, change := range map[string]func(*Spec){
		"leaf checks fail": func(s *Spec) { s.Check = func(context.Context) error { return errors.New("unit tests fail") } },
		"acceptance fails": func(s *Spec) { s.Acceptance = func(context.Context) error { return errors.New("e2e fails") } },
	} {
		s := spec
		change(&s)
		_, err := Move(ctx, s)
		var r *Refused
		if !errors.As(err, &r) {
			t.Errorf("%s: Move() = %v, want *Refused", name, err)
		}
		if got := gitlink(t, parent.Dir, "repos/api"); got != before {
			t.Errorf("%s: the pointer moved", name)
		}
	}
}

func TestMoveCommitsGeneratedFilesUnderTheShrinkGuard(t *testing.T) {
	_, parent, spec := landed(t)
	repos := filepath.Join(parent.Dir, "REPOS.md")
	write := func(rows int) {
		t.Helper()
		if err := os.WriteFile(repos, []byte(strings.Repeat("| repo | api |\n", rows)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(10)
	if _, err := git.Run(ctx, parent.Dir, "add", "REPOS.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, parent.Dir, "commit", "-q", "-m", "REPOS.md"); err != nil {
		t.Fatal(err)
	}
	spec.Generated = []string{"REPOS.md"}
	write(3)
	_, err := Move(ctx, spec)
	var se *shrink.Error
	if !errors.As(err, &se) {
		t.Fatalf("Move() = %v, want the shrink guard's refusal", err)
	}
	if got := gitlink(t, parent.Dir, "repos/api"); got == spec.To {
		t.Error("the pointer moved past the shrink guard")
	}
	spec.SkipShrink = true // the operator approved shrink-guard for this move
	if _, err := Move(ctx, spec); err != nil {
		t.Fatalf("Move() with the guard approved = %v", err)
	}
	if files, _ := git.Run(ctx, parent.Dir, "show", "--name-only", "--format=", "HEAD"); files != "REPOS.md\nrepos/api" {
		t.Errorf("the move commits %q, want the generated file and the pointer", files)
	}
}

func TestMoveRefusesOtherStagedChanges(t *testing.T) {
	_, parent, spec := landed(t)
	if err := os.WriteFile(filepath.Join(parent.Dir, "notes.md"), []byte("a person's note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, parent.Dir, "add", "notes.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := Move(ctx, spec); err == nil || !strings.Contains(err.Error(), "notes.md") {
		t.Errorf("Move() = %v, want a refusal naming the staged notes.md", err)
	}
	if staged, _ := git.Run(ctx, parent.Dir, "diff", "--cached", "--name-only"); staged != "notes.md" {
		t.Errorf("the person's staged change was touched: %q", staged)
	}
}
