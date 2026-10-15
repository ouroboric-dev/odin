// Package merge is the merge gate (spec §4.5). A branch can pass every check and then stop
// building after its base moves underneath it, so a run's branch lands on a leaf's main only
// through these steps: lock the target, merge the current base into the branch, re-run the
// checks on the result, and move main to the checked commit only if the branch head is still that
// commit and main is still the base that was merged.
package merge

import (
	"context"
	"fmt"

	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
)

// Spec is what to land.
type Spec struct {
	Leaf   *leaf.Leaf
	Branch string                          // the run's branch, checked out in the leaf
	Base   string                          // the branch it lands on, such as main
	Lock   string                          // the target's lock file, in <L1>/.factory/locks/
	Check  func(ctx context.Context) error // runs the checks on the commit the leaf has checked out
}

// Result is what landed.
type Result struct {
	Base    string // the base that was merged into the branch
	Checked string // the commit the checks passed on
	Merged  string // the commit Base now points at: Checked
}

// Why the gate refused.
const (
	WhyConflict = "conflict" // the base doesn't merge cleanly into the branch
	WhyChecks   = "checks"   // the checks fail on the branch with the base merged in
	WhyMoved    = "moved"    // the branch head or the base moved after the checks
)

// Refused is a gate that didn't land. The work goes back to the loop; nothing moved on Base.
type Refused struct {
	Why string
	Err error
}

func (r *Refused) Error() string { return fmt.Sprintf("merge gate refused (%s): %v", r.Why, r.Err) }
func (r *Refused) Unwrap() error { return r.Err }

// Land runs the gate. It returns a *Refused when the work must go back to the loop, and
// lock.ErrHeld when another process holds the target.
func Land(ctx context.Context, s Spec) (Result, error) {
	release, err := lock.Take(s.Lock)
	if err != nil {
		return Result{}, err
	}
	defer release()
	l := s.Leaf
	var res Result
	if res.Base, err = l.Rev(ctx, s.Base); err != nil {
		return res, err
	}
	if _, err := l.Git(ctx, "switch", "-q", s.Branch); err != nil {
		return res, err
	}
	msg := fmt.Sprintf("Merge %s into %s", s.Base, s.Branch)
	if _, err := l.Git(ctx, "merge", "-q", "--no-edit", "-m", msg, res.Base); err != nil {
		if _, abortErr := l.Git(ctx, "merge", "--abort"); abortErr != nil {
			return res, fmt.Errorf("%v; and the merge didn't abort: %v", err, abortErr)
		}
		return res, &Refused{Why: WhyConflict, Err: err}
	}
	if res.Checked, err = l.Head(ctx); err != nil {
		return res, err
	}
	if err := s.Check(ctx); err != nil {
		return res, &Refused{Why: WhyChecks, Err: err}
	}
	head, err := l.Rev(ctx, s.Branch)
	if err != nil {
		return res, err
	}
	if head != res.Checked {
		return res, &Refused{Why: WhyMoved, Err: fmt.Errorf("%s is at %s, not the checked %s", s.Branch, head, res.Checked)}
	}
	// update-ref with the old value moves the base only if it is still the base that was merged.
	if _, err := l.Git(ctx, "update-ref", "refs/heads/"+s.Base, res.Checked, res.Base); err != nil {
		return res, &Refused{Why: WhyMoved, Err: fmt.Errorf("%s moved during the checks: %w", s.Base, err)}
	}
	res.Merged = res.Checked
	return res, nil
}
