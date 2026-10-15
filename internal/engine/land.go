package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/merge"
	"github.com/ouroboric-dev/odin/internal/pointer"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/shrink"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// runMerge lands the run's branch of n's leaf on the leaf's main through the merge gate (spec
// §4.5). The gate's checks are n's pack checks, run in the leaf on the branch with main merged in.
// The merged commit becomes the leaf's checkpoint. A refusal fails the node: the work goes back to
// the loop (Phase 4), and a resume starts the gate again from the last checkpoint.
func runMerge(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	l, err := r.Leaf(n.Repo)
	if err != nil {
		return err
	}
	rec := r.State.Leaves[n.Repo]
	began := time.Now()
	res, landErr := merge.Land(ctx, merge.Spec{Leaf: l, Branch: rec.Branch, Base: "main", Lock: r.LockFile(n.Repo),
		Check: func(ctx context.Context) error { return runChecks(ctx, r, n, attempt, n.Checks, l.Dir()) }})
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt,
		DurationMS: time.Since(began).Milliseconds()}
	if landErr != nil {
		return failLanding(r, ev, landErr)
	}
	rec.Checkpoint = res.Merged
	r.State.Leaves[n.Repo] = rec
	ev.Result, ev.Commit = "merged into main", res.Merged
	if err := r.Log(ev); err != nil {
		return err
	}
	r.State.Summaries[n.ID] = fmt.Sprintf("merged %s into main at %s", rec.Branch, res.Merged)
	return nil
}

// runPointer moves n's leaf pointer in the leaf's parent umbrella to the leaf's main, one level
// (spec §3.4), under the parent's lock: n's pack checks run in the leaf at that commit, its
// acceptance checks in the parent, and the shrink guard reads the parent's generated files. A
// shrunk file or failed checks fail the node.
func runPointer(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	nodes, err := tree.Resolve(ctx, r.Setup.Root, filepath.Join(r.Setup.ProjectPath, n.Repo))
	if err != nil {
		return err
	}
	parent, leafNode := nodes[len(nodes)-2], nodes[len(nodes)-1]
	child, err := filepath.Rel(parent.Dir, leafNode.Dir)
	if err != nil {
		return err
	}
	parentRepo, err := filepath.Rel(r.Project, parent.Dir)
	if err != nil {
		return err
	}
	l, err := r.Leaf(n.Repo)
	if err != nil {
		return err
	}
	to, err := l.Rev(ctx, "main")
	if err != nil {
		return err
	}
	s := r.State
	began := time.Now()
	res, moveErr := pointer.Move(ctx, pointer.Spec{Parent: parent, Child: child, Leaf: l, To: to,
		Lock:       r.LockFile(parentRepo),
		Check:      func(ctx context.Context) error { return runChecks(ctx, r, n, attempt, n.Checks, l.Dir()) },
		Acceptance: func(ctx context.Context) error { return runChecks(ctx, r, n, attempt, n.Acceptance, parent.Dir) },
		Generated:  n.Generated,
		Message:    fmt.Sprintf("Move %s to %.12s (run %s, node %s)", child, to, s.ID, n.ID)})
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt,
		DurationMS: time.Since(began).Milliseconds()}
	if moveErr != nil {
		return failLanding(r, ev, moveErr)
	}
	if res.Commit != "" {
		moved := gungnir.Event{Name: gungnir.EventPointerMoved, Node: n.ID, Attempt: attempt, Commit: res.Commit,
			Note: fmt.Sprintf("%s: %.12s -> %.12s", filepath.Join(parentRepo, child), res.From, res.To)}
		if err := r.Log(moved); err != nil {
			return err
		}
	}
	ev.Result, ev.Commit = "moved "+child, res.Commit
	if err := r.Log(ev); err != nil {
		return err
	}
	s.Summaries[n.ID] = fmt.Sprintf("moved %s to %s", child, to)
	return nil
}

// failLanding fails a merge or pointer node that the gate or the shrink guard refused, or whose
// target another process holds. Anything else is a failure of Odin itself.
func failLanding(r *run.Run, ev gungnir.Event, err error) error {
	var mr *merge.Refused
	var pr *pointer.Refused
	var se *shrink.Error
	if !errors.As(err, &mr) && !errors.As(err, &pr) && !errors.As(err, &se) && !errors.Is(err, lock.ErrHeld) {
		return err
	}
	ev.Name, ev.Result = gungnir.EventNodeFailed, err.Error()
	if lerr := r.Log(ev); lerr != nil {
		return lerr
	}
	return &nodeFailure{err}
}
