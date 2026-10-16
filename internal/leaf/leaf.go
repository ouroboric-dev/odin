// Package leaf runs host git on a leaf (spec §3.3, §4.3): the run's branch, checkpoint commits and
// resets. Agents work in a leaf, so nothing in it is trusted: git always gets the git dir Odin
// found in the tree, and a checkpoint refuses a planted git repository or a rewritten .git file.
package leaf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// Leaf is a leaf's working copy and the git dir its parent umbrella holds for it.
type Leaf struct {
	repo git.Repo
}

// PolicyError is work in a leaf that breaks a sandbox rule. Odin commits none of it, and the run
// stops at the hard approval point policy-violation.
type PolicyError struct{ Reason string }

func (e *PolicyError) Error() string { return "policy violation: " + e.Reason }

// Open returns the leaf of a checked-out leaf node.
func Open(n tree.Node) (*Leaf, error) {
	if n.Kind != schema.ChildLeaf || !n.Present {
		return nil, fmt.Errorf("%s is not a checked-out leaf", n.Path)
	}
	return &Leaf{repo: git.Repo{GitDir: n.GitDir, WorkTree: n.Dir}}, nil
}

// Dir is the leaf's working copy.
func (l *Leaf) Dir() string { return l.repo.WorkTree }

// Head returns the commit the leaf has checked out.
func (l *Leaf) Head(ctx context.Context) (string, error) { return l.Rev(ctx, "HEAD") }

// Rev returns the commit ref names.
func (l *Leaf) Rev(ctx context.Context, ref string) (string, error) {
	return l.repo.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// Git runs git on the leaf.
func (l *Leaf) Git(ctx context.Context, args ...string) (string, error) {
	return l.repo.Run(ctx, args...)
}

// StartBranch creates branch at from and checks it out. It refuses a leaf with uncommitted
// changes, so a run never discards work it didn't make.
func (l *Leaf) StartBranch(ctx context.Context, branch, from string) (string, error) {
	if err := l.checkGitFile(); err != nil {
		return "", err
	}
	if st, err := l.repo.Run(ctx, "status", "--porcelain"); err != nil {
		return "", err
	} else if st != "" {
		return "", fmt.Errorf("the leaf %s has uncommitted changes; commit or remove them first:\n%s", l.Dir(), st)
	}
	if _, err := l.repo.Run(ctx, "switch", "-q", "-c", branch, from); err != nil {
		return "", err
	}
	return l.Head(ctx)
}

// Checkpoint commits everything in the leaf to the branch it is on, and returns the head. It
// commits nothing, and committed is false, when nothing changed. A nested git repository or a
// rewritten .git file is a *PolicyError, and nothing is committed.
func (l *Leaf) Checkpoint(ctx context.Context, msg string) (head string, committed bool, err error) {
	if err := l.checkGitFile(); err != nil {
		return "", false, err
	}
	if err := l.checkNested(); err != nil {
		return "", false, err
	}
	if _, err := l.repo.Run(ctx, "add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := l.repo.Run(ctx, "diff", "--cached", "--quiet"); err == nil {
		head, err := l.Head(ctx)
		return head, false, err
	}
	if _, err := l.repo.Run(ctx, "commit", "-q", "-m", msg); err != nil {
		return "", false, err
	}
	head, err = l.Head(ctx)
	return head, true, err
}

// Save keeps what a reset to since would discard: when the leaf has changes, or its branch has
// moved past since, it creates ref (which must not exist) at a commit holding the leaf as it is,
// whose parent is the branch head. Ignored files aren't saved, and a reset keeps them. saved is
// false when there was nothing to keep. A planted repository is a *PolicyError, and nothing is saved.
func (l *Leaf) Save(ctx context.Context, ref, since, msg string) (saved bool, err error) {
	if err := l.restoreGitFile(); err != nil {
		return false, err
	}
	if err := l.checkNested(); err != nil {
		return false, err
	}
	head, err := l.Head(ctx)
	if err != nil {
		return false, err
	}
	st, err := l.repo.Run(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if st == "" && head == since {
		return false, nil
	}
	commit := head
	if st != "" {
		if _, err := l.repo.Run(ctx, "add", "-A"); err != nil {
			return false, err
		}
		tree, err := l.repo.Run(ctx, "write-tree")
		if err != nil {
			return false, err
		}
		if commit, err = l.repo.Run(ctx, "commit-tree", tree, "-p", head, "-m", msg); err != nil {
			return false, err
		}
	}
	// An empty old value: create the ref, never overwrite one.
	if _, err := l.repo.Run(ctx, "update-ref", "-m", msg, ref, commit, ""); err != nil {
		return false, err
	}
	return true, nil
}

// Guard refuses a leaf that breaks a sandbox rule — a rewritten .git file, or a planted git
// repository below its top level — without committing anything. A merge gate's or pointer move's
// checks run leaf code, so Odin re-checks the leaf before it runs parent git on it (decision 3, 9).
func (l *Leaf) Guard() error {
	if err := l.checkGitFile(); err != nil {
		return err
	}
	return l.checkNested()
}

// Reset puts the leaf back to commit sha on branch: the branch is moved to sha and checked out,
// changes and untracked files are removed, and the .git file is restored. Ignored files stay: they
// are never committed, and may be a person's own (.env, node_modules). Save first what a person may
// want back.
func (l *Leaf) Reset(ctx context.Context, branch, sha string) error {
	if err := l.restoreGitFile(); err != nil {
		return err
	}
	if _, err := l.repo.Run(ctx, "switch", "-q", "-f", "-C", branch, sha); err != nil {
		return err
	}
	_, err := l.repo.Run(ctx, "clean", "-q", "-ffd")
	return err
}

// checkGitFile refuses a leaf whose .git no longer points at the git dir Odin holds for it.
func (l *Leaf) checkGitFile() error {
	b, err := os.ReadFile(filepath.Join(l.Dir(), ".git"))
	if err != nil {
		return &PolicyError{fmt.Sprintf("the leaf's .git is not the file git made: %v", err)}
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return &PolicyError{"the leaf's .git file was changed"}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(l.Dir(), target)
	}
	got, err1 := filepath.EvalSymlinks(target)
	want, err2 := filepath.EvalSymlinks(l.repo.GitDir)
	if err1 != nil || err2 != nil || got != want {
		return &PolicyError{fmt.Sprintf("the leaf's .git file points at %s, not %s", target, l.repo.GitDir)}
	}
	return nil
}

func (l *Leaf) restoreGitFile() error {
	rel, err := filepath.Rel(l.Dir(), l.repo.GitDir)
	if err != nil {
		return err
	}
	p := filepath.Join(l.Dir(), ".git")
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return &PolicyError{"the leaf's .git is a directory"}
	}
	return os.WriteFile(p, []byte("gitdir: "+rel+"\n"), 0o644)
}

// checkNested refuses any .git below the leaf's top level: an agent can build a repository from
// plain file writes, and git would record it as a submodule (spec §3.3 rule 5).
func (l *Leaf) checkNested() error {
	top := filepath.Join(l.Dir(), ".git")
	return filepath.WalkDir(l.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() != ".git" || p == top {
			return nil
		}
		rel, _ := filepath.Rel(l.Dir(), filepath.Dir(p))
		return &PolicyError{fmt.Sprintf("the leaf %s holds a git repository at %s", l.Dir(), rel)}
	})
}

// IsPolicy reports whether err is a *PolicyError.
func IsPolicy(err error) bool {
	var pe *PolicyError
	return errors.As(err, &pe)
}
