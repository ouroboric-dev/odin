// Package pointer moves a leaf's pointer in its parent umbrella (spec §3.4). A move means "this
// child, at this commit, passed this level's checks", so it spans one level only: the parent gets
// one commit, and nothing above it moves.
package pointer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/shrink"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// Spec is the move.
type Spec struct {
	Parent     tree.Node                       // the umbrella whose pointer moves
	Child      string                          // the leaf's path in it
	Leaf       *leaf.Leaf                      // the leaf, checked out at To
	To         string                          // the commit the pointer moves to
	Lock       string                          // the parent's lock file
	Check      func(ctx context.Context) error // the leaf's checks, at To
	Acceptance func(ctx context.Context) error // the parent's acceptance tests, with the leaf at To
	Generated  []string                        // generated files of the parent, committed with the move
	SkipShrink bool                            // the operator approved shrink-guard for this move
	Message    string                          // the commit message
}

// Result is the move made.
type Result struct {
	From, To string // the pointer before and after
	Commit   string // the parent's commit; "" when the pointer was already at To
}

// Refused is a move whose checks failed. Nothing was committed.
type Refused struct {
	Why string
	Err error
}

func (r *Refused) Error() string { return fmt.Sprintf("pointer move refused (%s): %v", r.Why, r.Err) }
func (r *Refused) Unwrap() error { return r.Err }

// Move moves the pointer under the parent's lock. A generated file under half its rows is a
// *shrink.Error unless SkipShrink is set; failed checks are a *Refused. Either way nothing moves.
func Move(ctx context.Context, s Spec) (Result, error) {
	release, err := lock.Take(s.Lock)
	if err != nil {
		return Result{}, err
	}
	defer release()
	p := s.Parent.Dir
	res := Result{To: s.To}
	if res.From, err = git.Run(ctx, p, "rev-parse", "HEAD:"+s.Child); err != nil {
		return res, err
	}
	if res.From == s.To {
		return res, nil
	}
	if head, err := s.Leaf.Head(ctx); err != nil || head != s.To {
		return res, fmt.Errorf("the leaf %s is at %s, not %s; check it out at the commit the pointer moves to", s.Child, head, s.To)
	}
	if staged, err := git.Run(ctx, p, "diff", "--cached", "--name-only"); err != nil {
		return res, err
	} else if staged != "" {
		return res, fmt.Errorf("the umbrella %s has staged changes Odin didn't make (%s); commit or unstage them first", p, strings.ReplaceAll(staged, "\n", ", "))
	}
	if err := s.Check(ctx); err != nil {
		return res, &Refused{Why: "checks", Err: err}
	}
	if err := s.Acceptance(ctx); err != nil {
		return res, &Refused{Why: "acceptance", Err: err}
	}
	if !s.SkipShrink {
		for _, f := range s.Generated {
			prev, _ := git.Run(ctx, p, "show", "HEAD:"+f) // a file new in this move has no rows
			next, err := os.ReadFile(filepath.Join(p, f))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return res, err
			}
			if err := shrink.Check(f, []byte(prev), next); err != nil {
				return res, err
			}
		}
	}
	paths := append([]string{s.Child}, s.Generated...)
	if _, err := git.Run(ctx, p, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return res, err
	}
	if _, err := git.Run(ctx, p, append([]string{"commit", "-q", "-m", s.Message, "--"}, paths...)...); err != nil {
		return res, err
	}
	res.Commit, err = git.Run(ctx, p, "rev-parse", "HEAD")
	return res, err
}
