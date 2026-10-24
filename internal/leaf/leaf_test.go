package leaf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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

// The secret scan gets its files from Pending, so Pending must be exactly what the checkpoint
// commits with content: changed, new and staged files, links, odd names; never an ignored or a
// deleted file, or one the leaf's own git settings would hide.
func TestTheScanCoversExactlyWhatTheCheckpointCommits(t *testing.T) {
	l := api(t)
	write(t, l, ".gitignore", ".env\nbuild/\n")
	write(t, l, "tracked.txt", "v1\n")
	write(t, l, "gone.txt", "bye\n")
	write(t, l, "old-name.txt", "moved\n")
	write(t, l, "becomes-link.txt", "a file\n")
	write(t, l, "build/forced.txt", "tracked though ignored\n")
	if _, err := l.Git(ctx, "add", "-f", "build/forced.txt"); err != nil {
		t.Fatal(err)
	}
	cp, _, err := l.Checkpoint(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	write(t, l, "tracked.txt", "v2\n")
	write(t, l, "new/deep.txt", "new\n")
	write(t, l, " lead.txt", "a name with a space first\n")
	write(t, l, "odd\nname.txt", "a newline in the name\n")
	write(t, l, ".env", "SECRET=ignored\n")
	write(t, l, "build/forced.txt", "changed\n")
	write(t, l, "staged.txt", "staged by a person\n")
	if _, err := l.Git(ctx, "add", "staged.txt"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(l.Dir(), "gone.txt"))
	os.Rename(filepath.Join(l.Dir(), "old-name.txt"), filepath.Join(l.Dir(), "new-name.txt"))
	os.Remove(filepath.Join(l.Dir(), "becomes-link.txt"))
	os.Symlink("tracked.txt", filepath.Join(l.Dir(), "becomes-link.txt"))
	os.Symlink("/etc/hostname", filepath.Join(l.Dir(), "link"))
	// An ignore file a git that reads the user's settings would apply: it would hide every .txt.
	xdg := t.TempDir()
	os.MkdirAll(filepath.Join(xdg, "git"), 0o755)
	os.WriteFile(filepath.Join(xdg, "git", "ignore"), []byte("*.txt\n"), 0o644)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	files, err := l.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending() = %v", err)
	}
	want := []string{" lead.txt", "becomes-link.txt", "build/forced.txt", "link", "new-name.txt", "new/deep.txt", "odd\nname.txt", "staged.txt", "tracked.txt"}
	if !slices.Equal(files, want) {
		t.Errorf("Pending() = %q\nwant %q", files, want)
	}
	head, committed, err := l.Checkpoint(ctx, "the node's work")
	if err != nil || !committed {
		t.Fatal(err)
	}
	out, err := l.repo.Output(ctx, "diff-tree", "-r", "-z", "--no-renames", "--name-only", "--diff-filter=d", cp, head)
	if err != nil {
		t.Fatal(err)
	}
	commits := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	slices.Sort(commits)
	if !slices.Equal(files, commits) {
		t.Errorf("the scan covers %q\nthe checkpoint committed %q", files, commits)
	}
}

// A .git that is not a regular file is refused before Odin reads it: a link, even to a file that
// names the right git dir, or a named pipe, which would block the read.
func TestAGitFileThatIsNotARegularFileIsAPolicyViolation(t *testing.T) {
	plant := map[string]func(t *testing.T, l *Leaf, p string){
		"symlink": func(t *testing.T, l *Leaf, p string) {
			right := filepath.Join(t.TempDir(), "gitfile")
			if err := os.WriteFile(right, []byte("gitdir: "+l.repo.GitDir+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(right, p); err != nil {
				t.Fatal(err)
			}
		},
		"named pipe": func(t *testing.T, l *Leaf, p string) {
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	checks := map[string]func(l *Leaf) error{
		"Checkpoint": func(l *Leaf) error { _, _, err := l.Checkpoint(ctx, "after a .git swap"); return err },
		"Guard":      func(l *Leaf) error { return l.Guard() },
	}
	for what, plant := range plant {
		for name, check := range checks {
			t.Run(what+"/"+name, func(t *testing.T) {
				l := api(t)
				before, _ := l.Head(ctx)
				write(t, l, "new.txt", "work\n")
				p := filepath.Join(l.Dir(), ".git")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				plant(t, l, p)
				done := make(chan error, 1)
				go func() { done <- check(l) }()
				select {
				case err := <-done:
					if !IsPolicy(err) {
						t.Errorf("%s() with a .git that is a %s = %v, want a *PolicyError", name, what, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s() with a .git that is a %s did not return", name, what)
				}
				if after, _ := l.Rev(ctx, "ygg/run1"); after != before {
					t.Errorf("%s() committed", name)
				}
			})
		}
	}
}

// A .git that is a link is refused, and restoring the .git file never writes to what it points at.
func TestRestoringAGitFileNeverWritesThroughASymlink(t *testing.T) {
	restores := map[string]func(l *Leaf, cp string) error{
		"Reset": func(l *Leaf, cp string) error { return l.Reset(ctx, "ygg/run1", cp) },
		"Save": func(l *Leaf, cp string) error {
			_, err := l.Save(ctx, "refs/ygg/run1/discarded/1", cp, "save")
			return err
		},
	}
	for name, restore := range restores {
		t.Run(name, func(t *testing.T) {
			l := api(t)
			cp, _ := l.Head(ctx)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("not the leaf's\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(l.Dir(), ".git")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Fatal(err)
			}
			if err := restore(l, cp); !IsPolicy(err) {
				t.Errorf("%s() with a symlinked .git = %v, want a *PolicyError", name, err)
			}
			if b, _ := os.ReadFile(outside); string(b) != "not the leaf's\n" {
				t.Errorf("%s() wrote through the symlink: the target holds %q", name, b)
			}
			if st, err := os.Lstat(p); err != nil || st.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s() replaced the symlink, which is the evidence: %v", name, err)
			}
		})
	}
	t.Run("hard link", func(t *testing.T) {
		l := api(t)
		cp, _ := l.Head(ctx)
		p := filepath.Join(l.Dir(), ".git")
		other := filepath.Join(filepath.Dir(l.Dir()), "linked")
		if err := os.WriteFile(other, []byte("not the leaf's\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, p); err != nil {
			t.Fatal(err)
		}
		if err := l.Reset(ctx, "ygg/run1", cp); err != nil {
			t.Fatalf("Reset() with a hard-linked .git = %v", err)
		}
		if b, _ := os.ReadFile(other); string(b) != "not the leaf's\n" {
			t.Errorf("Reset() wrote through the hard link: the other name holds %q", b)
		}
		if _, _, err := l.Checkpoint(ctx, "after the reset"); err != nil {
			t.Errorf("the reset didn't restore the leaf's .git: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		l := api(t)
		cp, _ := l.Head(ctx)
		if err := os.Remove(filepath.Join(l.Dir(), ".git")); err != nil {
			t.Fatal(err)
		}
		if err := l.Reset(ctx, "ygg/run1", cp); err != nil {
			t.Fatalf("Reset() with no .git = %v", err)
		}
		if _, _, err := l.Checkpoint(ctx, "after the reset"); err != nil {
			t.Errorf("the reset didn't restore the leaf's .git: %v", err)
		}
	})
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
	if _, err := os.Stat(filepath.Join(l.Dir(), "new.txt")); err == nil {
		t.Error("new.txt survived the reset")
	}
	// Ignored files are never committed, and may be a person's (.env, node_modules): a reset keeps them.
	if _, err := os.Stat(filepath.Join(l.Dir(), "build/out.bin")); err != nil {
		t.Errorf("the reset deleted an ignored file: %v", err)
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

// What a reset would discard, a person's fix or a commit on the run's branch after the checkpoint,
// is saved to a ref first.
func TestSaveKeepsWhatAResetWouldDiscard(t *testing.T) {
	l := api(t)
	write(t, l, ".gitignore", "build/\n")
	cp, _, err := l.Checkpoint(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := l.Save(ctx, "refs/ygg/run1/discarded/1", cp, "nothing to save"); err != nil || saved {
		t.Errorf("Save() of a leaf at its checkpoint = %v, %v; want nothing saved", saved, err)
	}
	write(t, l, "committed.txt", "a commit after the checkpoint\n")
	after, _, err := l.Checkpoint(ctx, "after the checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	write(t, l, "fixed.txt", "a person's fix\n")
	write(t, l, "build/out.bin", "ignored\n")
	saved, err := l.Save(ctx, "refs/ygg/run1/discarded/1", cp, "before resetting to the checkpoint")
	if err != nil || !saved {
		t.Fatalf("Save() = %v, %v", saved, err)
	}
	if files, _ := l.Git(ctx, "ls-tree", "-r", "--name-only", "refs/ygg/run1/discarded/1"); !strings.Contains(files, "fixed.txt") || !strings.Contains(files, "committed.txt") || strings.Contains(files, "out.bin") {
		t.Errorf("the saved commit holds %q, want the fix and the later commit, and no ignored file", files)
	}
	if parent, _ := l.Rev(ctx, "refs/ygg/run1/discarded/1^"); parent != after {
		t.Errorf("the saved commit's parent = %s, want the branch head %s, so its commits stay reachable", parent, after)
	}
	if head, _ := l.Head(ctx); head != after {
		t.Error("Save() moved the branch")
	}
	if _, err := l.Save(ctx, "refs/ygg/run1/discarded/1", cp, "again"); err == nil {
		t.Error("Save() overwrote an existing ref")
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

func TestTidyRemovesWhatChecksWroteAndKeepsIgnoredFilesAndEvidence(t *testing.T) {
	l := api(t)
	write(t, l, ".gitignore", ".env\n")
	if _, _, err := l.Checkpoint(ctx, "ignore"); err != nil {
		t.Fatal(err)
	}
	write(t, l, "junk.txt", "scratch\n")
	write(t, l, ".gitignore", "changed by a check\n")
	write(t, l, ".env", "KEEP=1\n")
	if err := l.Tidy(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := l.Git(ctx, "status", "--porcelain"); st != "" {
		t.Errorf("after Tidy: %q", st)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), ".env")); err != nil {
		t.Error("Tidy removed an ignored file, which may be a person's own")
	}
	write(t, l, "vendor/x/.git/HEAD", "ref: refs/heads/main\n")
	write(t, l, "junk.txt", "again\n")
	if err := l.Tidy(ctx); !IsPolicy(err) {
		t.Errorf("Tidy with a planted repository = %v, want a *PolicyError", err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), "vendor/x/.git/HEAD")); err != nil {
		t.Error("Tidy erased the planted repository, which is the evidence")
	}
}

// An exclude rule in the leaf's git dir keeps what it names out of a checkpoint and the scan's list, and a reset
// keeps it; the rule is written once.
func TestAnExcludedFolderIsNeverCommitted(t *testing.T) {
	l := api(t)
	for range 2 {
		if err := l.Exclude("/.ygg-inputs/"); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(l.repo.GitDir, "info", "exclude"))
	if err != nil || strings.Count(string(b), "/.ygg-inputs/\n") != 1 {
		t.Fatalf("info/exclude = %q, %v; want the rule once", b, err)
	}
	write(t, l, ".ygg-inputs/change.md", "the change\n")
	write(t, l, "src/app.go", "package app\n")
	if files, err := l.Pending(ctx); err != nil || !slices.Equal(files, []string{"src/app.go"}) {
		t.Errorf("Pending() = %v, %v", files, err)
	}
	sha, _, err := l.Checkpoint(ctx, "ygg: run1 node build attempt 1")
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := l.repo.Run(ctx, "show", "--name-only", "--format=", sha); files != "src/app.go" {
		t.Errorf("the checkpoint holds %q", files)
	}
	if err := l.Tidy(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir(), ".ygg-inputs", "change.md")); err != nil {
		t.Errorf("a tidy removed the excluded file: %v", err)
	}
}
