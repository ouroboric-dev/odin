package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/ledger"
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
		Check: func(ctx context.Context) error {
			return tidied(ctx, l, runChecks(ctx, r, n, attempt, n.Checks, n.Repo, l.Dir()))
		}})
	var hs *hardStop
	if errors.As(landErr, &hs) { // the sandbox broke a hardening rule
		return hs
	}
	if leaf.IsPolicy(landErr) { // a check rewrote the leaf's .git or planted a repository
		return &hardStop{gate: gungnir.GatePolicyViolation, err: landErr}
	}
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt, Task: n.Task,
		DurationMS: time.Since(began).Milliseconds()}
	if landErr != nil {
		if workRefused(landErr) && res.Checked != "" {
			// The branch, with the base merged in, is what the checks refused: the loop goes on from there.
			rec.Checkpoint = res.Checked
			r.State.Leaves[n.Repo] = rec
		}
		return failLanding(r, n, ev, landErr)
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
// shrunk file stops the run at the hard approval point shrink-guard; failed checks fail the node.
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
	if judged, ok := r.State.Judged[n.ID]; ok && judged != to {
		// A pointer-contract gate judged the move to another commit; main moved on since (spec §3.4).
		return &nodeFailure{fmt.Errorf("main of %s is at %.12s, but the pointer-contract gate judged the move to %.12s; no command runs the gate again: move main of %s back to that commit and resume, or start a new run", n.Repo, to, judged, n.Repo)}
	}
	if err := shipRouting(r, n, attempt, parent); err != nil {
		return err
	}
	s := r.State
	began := time.Now()
	res, moveErr := pointer.Move(ctx, pointer.Spec{Parent: parent, Child: child, Leaf: l, To: to,
		Lock: r.LockFile(parentRepo),
		Check: func(ctx context.Context) error {
			return tidied(ctx, l, runChecks(ctx, r, n, attempt, n.Checks, n.Repo, l.Dir()))
		},
		Acceptance: func(ctx context.Context) error {
			if r.Sandbox == nil || len(n.Acceptance) == 0 {
				return runChecks(ctx, r, n, attempt, n.Acceptance, "", parent.Dir)
			}
			return runAcceptance(ctx, r, n, attempt, parentRepo, n.Repo, to)
		},
		Generated: n.Generated, SkipShrink: s.Waived[n.ID],
		Message: fmt.Sprintf("Move %s to %.12s (run %s, node %s)", child, to, s.ID, n.ID)})
	var hs *hardStop
	if errors.As(moveErr, &hs) { // the sandbox broke a hardening rule
		return hs
	}
	// A waiver is for the next move only: whatever came of it, a later move is not waived (backlog B-010).
	delete(s.Waived, n.ID)
	var se *shrink.Error
	if errors.As(moveErr, &se) {
		// The attempt the node_started line opened closes in the ledger (backlog B-010).
		fail := gungnir.Event{Name: gungnir.EventNodeFailed, Node: n.ID, Attempt: attempt, Task: n.Task, Result: moveErr.Error(),
			DurationMS: time.Since(began).Milliseconds()}
		if err := r.Log(fail); err != nil {
			return err
		}
		return &hardStop{gate: gungnir.GateShrinkGuard, err: moveErr}
	}
	if leaf.IsPolicy(moveErr) { // a check rewrote the leaf's .git or planted a repository
		return &hardStop{gate: gungnir.GatePolicyViolation, err: moveErr}
	}
	ev := gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt, Task: n.Task,
		DurationMS: time.Since(began).Milliseconds()}
	if moveErr != nil {
		return failLanding(r, n, ev, moveErr)
	}
	if res.Commit != "" {
		moved := gungnir.Event{Name: gungnir.EventPointerMoved, Node: n.ID, Attempt: attempt, Task: n.Task, Commit: res.Commit,
			Note: fmt.Sprintf("%s: %.12s -> %.12s", filepath.Join(parentRepo, child), res.From, res.To)}
		if err := r.Log(moved); err != nil {
			return err
		}
	} else if attempt > 1 {
		// The pointer is already there on a later attempt: an earlier attempt moved it, and the ledger may
		// not have the line, if the process died between the commit and the line (backlog B-010).
		logged, err := ledger.Has(filepath.Join(r.Dir, "ledger.jsonl"), func(e gungnir.Event) bool {
			return e.Name == gungnir.EventPointerMoved && e.Node == n.ID
		})
		if err != nil {
			return err
		}
		if !logged {
			moved := gungnir.Event{Name: gungnir.EventPointerMoved, Node: n.ID, Attempt: attempt, Task: n.Task,
				Note: fmt.Sprintf("%s: already at %.12s; an earlier attempt moved it and the run didn't record it", filepath.Join(parentRepo, child), res.To)}
			if err := r.Log(moved); err != nil {
				return err
			}
		}
	}
	ev.Result, ev.Commit = "moved "+child, res.Commit
	if err := r.Log(ev); err != nil {
		return err
	}
	s.Summaries[n.ID] = fmt.Sprintf("moved %s to %s", child, to)
	return nil
}

// runAcceptance runs a pointer node's acceptance checks, in a run with a sandbox, in the sandbox that holds
// a snapshot of the project (change realm-content): the lead realm's checks, on the lead realm's
// acceptance stack, over the committed files of every leaf, the one being moved at the commit it moves to.
// Nothing of a leaf's working copy or git dir is in it, and what the checks write stays in the snapshot.
func runAcceptance(ctx context.Context, r *run.Run, n schema.Node, attempt int, parentRepo, leafRepo, to string) error {
	if parentRepo != "." {
		return &nodeFailure{fmt.Errorf("node %s runs acceptance checks in the umbrella %s; the snapshot sandbox holds the project, so they run where the leaf's parent is the project", n.ID, parentRepo)}
	}
	runner, err := r.SnapshotRunner(ctx, n.ID, map[string]string{leafRepo: to})
	var se *run.SnapshotError
	if errors.As(err, &se) {
		return &nodeFailure{err}
	} else if err != nil {
		return sandboxStart(err)
	}
	return runChecksWith(ctx, r, n, attempt, n.Acceptance, r.Setup.GraphPack, func(argv []string, logPath string) (error, error) {
		return commandOn(ctx, runner, n.ID, argv, time.Duration(n.Timeout), logPath)
	})
}

// failLanding fails a merge or pointer node that the gate refused, or whose target another process
// holds. Anything else is a failure of Odin itself. A gate that refused because a check ran and failed on
// the work sends the work back into the loop that made it when the node names one (spec §4.5, back_to); a
// base that doesn't merge cleanly, a target another process holds, and a refusal for Odin's own failure
// under the checks (an interrupt, a sandbox that failed, a check the realm lacks, a leaf that couldn't be
// tidied) fail the node, since the implementer can't fix any of them (review I-5).
func failLanding(r *run.Run, n schema.Node, ev gungnir.Event, err error) error {
	var mr *merge.Refused
	var pr *pointer.Refused
	if !errors.As(err, &mr) && !errors.As(err, &pr) && !errors.Is(err, lock.ErrHeld) {
		return err
	}
	ev.Name, ev.Result = gungnir.EventNodeFailed, err.Error()
	if lerr := r.Log(ev); lerr != nil {
		return lerr
	}
	if workRefused(err) {
		if sent, serr := sendBack(r, n, err.Error()); sent || serr != nil {
			return serr
		}
	}
	return &nodeFailure{err}
}

// workRefused reports whether a gate refused the work because one of its checks ran and failed: a merge
// gate's checks, or a pointer move's checks or acceptance.
func workRefused(err error) bool {
	var mr *merge.Refused
	var pr *pointer.Refused
	var cf *checkFailed
	if !errors.As(err, &cf) {
		return false
	}
	return (errors.As(err, &mr) && mr.Why == merge.WhyChecks) || errors.As(err, &pr)
}

// tidied returns the checks' own result, after putting the leaf back to the commit they ran on, so
// no file a check wrote blocks the next run (backlog B-004). A leaf the checks left breaking a sandbox
// rule isn't cleaned: that is the error, for a hard stop.
func tidied(ctx context.Context, l *leaf.Leaf, checkErr error) error {
	if err := l.Tidy(ctx); err != nil {
		return err
	}
	return checkErr
}
