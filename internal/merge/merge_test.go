package merge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

// setup returns the api leaf on run branch ygg/run1 with one feature commit, and the spec to land
// it with check as its checks.
func setup(t *testing.T, check func(context.Context) error) (*leaf.Leaf, Spec) {
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
	commit(t, l, "feature.txt", "the feature\n")
	return l, Spec{Leaf: l, Branch: "ygg/run1", Base: "main",
		Lock: filepath.Join(nodes[1].Dir, ".factory", "locks", "repos-api.lock"), Check: check}
}

// commit writes path on the branch the leaf has checked out, and commits it.
func commit(t *testing.T, l *leaf.Leaf, path, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.Dir(), path), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	head, _, err := l.Checkpoint(ctx, "add "+path)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// moveMain puts a commit adding path on main while the run branch stays checked out, as a person
// working on main would.
func moveMain(t *testing.T, l *leaf.Leaf, path string) string {
	t.Helper()
	if _, err := l.Git(ctx, "switch", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	head := commit(t, l, path, "on main\n")
	if _, err := l.Git(ctx, "switch", "-q", "ygg/run1"); err != nil {
		t.Fatal(err)
	}
	return head
}

func exists(l *leaf.Leaf, path string) bool {
	_, err := os.Stat(filepath.Join(l.Dir(), path))
	return err == nil
}

// Spec §13: a branch that passed against an old base but fails against the new one MUST NOT merge.
func TestMergeGateRefusesABranchThatFailsAgainstTheNewBase(t *testing.T) {
	l, spec := setup(t, nil)
	spec.Check = func(context.Context) error {
		if exists(l, "feature.txt") && exists(l, "breaks-feature.txt") {
			return errors.New("the feature's tests fail with breaks-feature.txt")
		}
		return nil
	}
	if err := spec.Check(ctx); err != nil {
		t.Fatalf("the branch fails its checks before main moved: %v", err)
	}
	moved := moveMain(t, l, "breaks-feature.txt")
	_, err := Land(ctx, spec)
	var r *Refused
	if !errors.As(err, &r) || r.Why != WhyChecks {
		t.Fatalf("Land() = %v, want refused on the checks", err)
	}
	if main, _ := l.Rev(ctx, "main"); main != moved {
		t.Errorf("main = %s, want it unmoved at %s", main, moved)
	}
}

// Spec §13: a head that moved after review MUST abort.
func TestMergeGateAbortsWhenTheHeadMovedAfterReview(t *testing.T) {
	l, spec := setup(t, nil)
	base, _ := l.Rev(ctx, "main")
	spec.Check = func(context.Context) error {
		commit(t, l, "late.txt", "pushed after the checks started\n") // the head moves under the gate
		return nil
	}
	_, err := Land(ctx, spec)
	var r *Refused
	if !errors.As(err, &r) || r.Why != WhyMoved {
		t.Fatalf("Land() = %v, want refused because the head moved", err)
	}
	if main, _ := l.Rev(ctx, "main"); main != base {
		t.Errorf("main moved to %s", main)
	}
}

// A person's commit on main during the checks must survive: the gate moves main only if it is still
// the base it merged (update-ref's old value).
func TestMergeGateRefusesWhenMainMovesDuringTheChecks(t *testing.T) {
	l, spec := setup(t, nil)
	var person string
	spec.Check = func(ctx context.Context) error {
		base, _ := l.Rev(ctx, "main")
		var err error
		if person, err = l.Git(ctx, "commit-tree", base+"^{tree}", "-p", base, "-m", "a person's commit on main"); err != nil {
			return err
		}
		_, err = l.Git(ctx, "update-ref", "refs/heads/main", person)
		return err
	}
	_, err := Land(ctx, spec)
	var r *Refused
	if !errors.As(err, &r) || r.Why != WhyMoved {
		t.Fatalf("Land() = %v, want refused because main moved", err)
	}
	if main, _ := l.Rev(ctx, "main"); main != person {
		t.Errorf("main = %s, want the person's commit %s", main, person)
	}
}

func TestMergeGateLandsTheCheckedCommit(t *testing.T) {
	var checkedOn string
	l, spec := setup(t, nil)
	spec.Check = func(ctx context.Context) error {
		checkedOn, _ = l.Head(ctx)
		if !exists(l, "feature.txt") || !exists(l, "docs.txt") {
			return errors.New("the checks didn't run on the branch with the new base merged in")
		}
		return nil
	}
	moveMain(t, l, "docs.txt")
	res, err := Land(ctx, spec)
	if err != nil {
		t.Fatalf("Land() = %v", err)
	}
	if main, _ := l.Rev(ctx, "main"); main != res.Merged || res.Merged != checkedOn || res.Checked != checkedOn {
		t.Errorf("main = %s, result %+v, checked on %s; want main at the checked commit", main, res, checkedOn)
	}
	if lock.Held(spec.Lock) {
		t.Error("the gate kept its lock")
	}
}

func TestMergeGateRefusesAConflict(t *testing.T) {
	l, spec := setup(t, func(context.Context) error { return nil })
	before, _ := l.Head(ctx)
	if _, err := l.Git(ctx, "switch", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	commit(t, l, "feature.txt", "main's own version\n")
	if _, err := l.Git(ctx, "switch", "-q", "ygg/run1"); err != nil {
		t.Fatal(err)
	}
	_, err := Land(ctx, spec)
	var r *Refused
	if !errors.As(err, &r) || r.Why != WhyConflict {
		t.Fatalf("Land() = %v, want refused on a conflict", err)
	}
	if head, _ := l.Head(ctx); head != before {
		t.Errorf("the branch moved to %s after the aborted merge", head)
	}
	if st, _ := l.Git(ctx, "status", "--porcelain"); st != "" {
		t.Errorf("the aborted merge left changes:\n%s", st)
	}
}

func TestMergeGateWaitsForNoOne(t *testing.T) {
	l, spec := setup(t, func(context.Context) error { return nil })
	base, _ := l.Rev(ctx, "main")
	release, err := lock.Take(spec.Lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Land(ctx, spec); !errors.Is(err, lock.ErrHeld) {
		t.Errorf("Land() = %v, want lock.ErrHeld while another process holds the target", err)
	}
	if main, _ := l.Rev(ctx, "main"); main != base {
		t.Error("main moved without the lock")
	}
}
