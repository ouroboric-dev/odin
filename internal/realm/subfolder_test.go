package realm

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := git.Run(context.Background(), dir, args...); err != nil {
		t.Fatal(err)
	}
}

// A graph pack is one folder of a repo, such as software in the voluspa leaf. Its hash is of the files
// committed under it, named relative to it, so it is what the same files hash to as a folder of their own.
func TestASubfolderOfAGitRepoIsHashedFromItsCommittedFiles(t *testing.T) {
	base := t.TempDir()
	plain := filepath.Join(base, "plain")
	fixture(t, plain, "software", "0.1.0")
	want, err := Hash(plain)
	if err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(base, "repo")
	soft := filepath.Join(repo, "software")
	fixture(t, soft, "software", "0.1.0")
	fixture(t, filepath.Join(repo, "untracked"), "untracked", "0.1.0")
	fixture(t, filepath.Join(repo, "ignored"), "ignored", "0.1.0")
	write(t, repo, ".gitignore", "ignored/\n", 0o644)
	write(t, repo, "README.md", "the repo\n", 0o644)
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "add", "--", "software", ".gitignore", "README.md")
	gitIn(t, repo, "commit", "-q", "-m", "pack")

	t.Run("a clean subfolder hashes as a plain copy of it", func(t *testing.T) {
		if got, err := Hash(soft); err != nil || got != want {
			t.Errorf("Hash(software) = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("an untracked file in it leaves the hash unchanged", func(t *testing.T) {
		write(t, soft, "notes.md", "not committed\n", 0o644)
		write(t, soft, "dist/app.js", "built\n", 0o644)
		if got, err := Hash(soft); err != nil || got != want {
			t.Errorf("Hash(software) = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("a file elsewhere in the repo that changed leaves it loading with the same hash", func(t *testing.T) {
		write(t, repo, "README.md", "changed, not committed\n", 0o644)
		if got, err := Hash(soft); err != nil || got != want {
			t.Errorf("Hash(software) = %q, %v; want %q", got, err, want)
		}
		write(t, repo, "README.md", "the repo\n", 0o644)
	})
	t.Run("a folder with no committed file is refused, not hashed as empty", func(t *testing.T) {
		for _, name := range []string{"untracked", "ignored"} {
			if got, err := Hash(filepath.Join(repo, name)); err == nil || !strings.Contains(err.Error(), "none of its files is committed") {
				t.Errorf("Hash(%s) = %q, %v; want a refusal saying none of its files is committed", name, got, err)
			}
		}
	})
	t.Run("a changed tracked file in it is refused", func(t *testing.T) {
		write(t, soft, "prompts/implementer.md", "Changed, not committed.\n", 0o644)
		if got, err := Hash(soft); err == nil || !strings.Contains(err.Error(), "differs from the committed realm") || !strings.Contains(err.Error(), "prompts/implementer.md") {
			t.Errorf("Hash(software) = %q, %v; want a refusal naming the changed file", got, err)
		}
	})
}
