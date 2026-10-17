package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
)

var bg = context.Background()

// repo makes a git repo in a temp dir with the given files committed, and returns it.
func repo(t *testing.T, files map[string]string) git.Repo {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, files)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "files"}} {
		if _, err := git.Run(bg, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return git.Repo{GitDir: filepath.Join(dir, ".git"), WorkTree: dir}
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportHoldsTheCommittedFilesOfTheProjectAndOfEachLeafAndNothingElse(t *testing.T) {
	project := repo(t, map[string]string{"docs/readme.md": "docs\n", "run.sh": "#!/bin/sh\n"})
	web := repo(t, map[string]string{"src/app.ts": "app\n", ".gitignore": "node_modules\n", "e2e/run.sh": "#!/bin/sh\n"})
	api := repo(t, map[string]string{"main.go": "package main\n"})
	// What a leaf holds beyond its commit must not reach the snapshot: ignored build output, a file not
	// added yet, and a tracked file edited since.
	write(t, web.WorkTree, map[string]string{"node_modules/pkg/index.js": "big\n", "scratch.txt": "not added\n", "src/app.ts": "edited, not committed\n"})
	dst := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dst, map[string]string{"left-over.txt": "from the last export\n"})
	if err := Export(bg, dst, []Source{{Rel: ".", Repo: project}, {Rel: "repos/web", Repo: web}, {Rel: "repos/api", Repo: api}}); err != nil {
		t.Fatalf("Export() = %v", err)
	}
	want := map[string]string{"docs/readme.md": "docs\n", "run.sh": "#!/bin/sh\n", "repos/web/src/app.ts": "app\n", "repos/web/.gitignore": "node_modules\n",
		"repos/web/e2e/run.sh": "#!/bin/sh\n", "repos/api/main.go": "package main\n"}
	var got []string
	filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, rel)
			if body, ok := want[rel]; !ok {
				t.Errorf("the snapshot holds %s, which no repo committed", rel)
			} else if b, _ := os.ReadFile(p); string(b) != body {
				t.Errorf("%s = %q, want %q", rel, b, body)
			}
		}
		return nil
	})
	if len(got) != len(want) {
		t.Errorf("the snapshot holds %v, want exactly %d files", got, len(want))
	}
	for rel, exec := range map[string]bool{"run.sh": true, "repos/web/e2e/run.sh": true, "docs/readme.md": false} {
		st, err := os.Stat(filepath.Join(dst, rel))
		if err != nil || (st.Mode()&0o111 != 0) != exec {
			t.Errorf("%s: mode %v, %v; want executable %v", rel, st.Mode(), err, exec)
		}
	}
	st, _ := os.Stat(dst)
	if !st.IsDir() {
		t.Error("the folder itself was replaced")
	}
}

func TestExportRefusesWhatCouldLeadOutOfTheSnapshot(t *testing.T) {
	linked := repo(t, map[string]string{"a.txt": "a\n"})
	if err := os.Symlink("/etc/passwd", filepath.Join(linked.WorkTree, "passwd")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "a link"}} {
		if _, err := git.Run(bg, linked.WorkTree, args...); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	err := Export(bg, dst, []Source{{Rel: ".", Repo: linked}})
	if err == nil || !strings.Contains(err.Error(), "passwd") || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a tracked symlink: Export() = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "passwd")); err == nil {
		t.Error("the symlink was written")
	}
	plain := repo(t, map[string]string{"a.txt": "a\n"})
	for _, rel := range []string{"../out", "/abs", "a/../../b", ""} {
		if err := Export(bg, t.TempDir(), []Source{{Rel: rel, Repo: plain}}); err == nil {
			t.Errorf("Export() into %q = nil, want an error", rel)
		}
	}
	// The leaf's own .gitattributes cannot drop or rewrite what is committed.
	attr := repo(t, map[string]string{
		".gitattributes":   "e2e/fail.spec.ts export-ignore\nsubst.txt export-subst\n",
		"e2e/fail.spec.ts": "fail\n",
		"subst.txt":        "$Format:%H$\n",
	})
	adst := t.TempDir()
	if err := Export(bg, adst, []Source{{Rel: ".", Repo: attr}}); err != nil {
		t.Fatalf("Export() with .gitattributes = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(adst, "e2e/fail.spec.ts")); err != nil || string(b) != "fail\n" {
		t.Errorf("an export-ignore file: %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(adst, "subst.txt")); string(b) != "$Format:%H$\n" {
		t.Errorf("an export-subst file was rewritten: %q", b)
	}
	// Two repos that both hold a file at one path: the second never overwrites the first.
	other := repo(t, map[string]string{"a.txt": "other\n"})
	if err := Export(bg, t.TempDir(), []Source{{Rel: ".", Repo: plain}, {Rel: ".", Repo: other}}); err == nil {
		t.Error("two repos wrote one file and Export() = nil")
	}
}

func TestExportStopsAtMoreFilesOrBytesThanASnapshotMayHold(t *testing.T) {
	r := repo(t, map[string]string{"a.txt": "1\n", "b.txt": "2\n", "c.txt": "3\n"})
	defer func(f int, b int64) { maxFiles, maxBytes = f, b }(maxFiles, maxBytes)
	maxFiles = 2
	if err := Export(bg, t.TempDir(), []Source{{Rel: ".", Repo: r}}); err == nil || !strings.Contains(err.Error(), "more than 2 files") {
		t.Errorf("too many files: Export() = %v", err)
	}
	maxFiles, maxBytes = 100, 4
	if err := Export(bg, t.TempDir(), []Source{{Rel: ".", Repo: r}}); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("too many bytes: Export() = %v", err)
	}
}

func TestExportTakesAReposFilesAtTheCommitItWasToldTo(t *testing.T) {
	r := repo(t, map[string]string{"a.txt": "first\n"})
	first, err := git.Run(bg, r.WorkTree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.WorkTree, map[string]string{"a.txt": "second\n"})
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "second"}} {
		if _, err := git.Run(bg, r.WorkTree, args...); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	if err := Export(bg, dst, []Source{{Rel: ".", Repo: r, Rev: first}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "first\n" {
		t.Errorf("a.txt = %q, want the file at the first commit", b)
	}
	for _, rev := range []string{"--output=/tmp/x", "main", "HEAD~1", first[:12], "HEAD; touch x"} {
		if err := Export(bg, t.TempDir(), []Source{{Rel: ".", Repo: r, Rev: rev}}); err == nil {
			t.Errorf("Export() at %q = nil, want an error: only HEAD or a full hash is taken", rev)
		}
	}
}
