package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lstat is the look a listing would have taken at name in dir.
func lstat(t *testing.T, dir, name string) fs.FileInfo {
	t.Helper()
	st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A file listed as a regular file and then replaced by another file at the same name is refused: what is
// read must be the file the listing found, not whatever is at the name when it is read (review m-1).
func TestReadingAFileRefusesAFileReplacedAfterTheListing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("listed\n"), 0o644)
	listed := lstat(t, dir, "a.md")
	if b, err := readRegular(dir, "a.md", listed, 100); err != nil || string(b) != "listed\n" {
		t.Fatalf("readRegular(the listed file) = %q, %v", b, err)
	}
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("other\n"), 0o644)
	if err := os.Rename(filepath.Join(dir, "b.md"), filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if b, err := readRegular(dir, "a.md", listed, 100); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("readRegular(a replaced file) = %q, %v; want a refusal saying it changed", b, err)
	}
}

// A file is checked when a folder is listed and read later. What is read must be the regular file the
// listing found: a symlink or anything else put at its name in between is refused, not followed (review
// M-2). The tests call the read directly on what a swap would leave.
func TestReadingAFileRefusesWhatIsNotARegularFileOfTheProject(t *testing.T) {
	dir, outside := t.TempDir(), outsideDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "docs", "ok.md"), []byte("fine\n"), 0o644)
	if b, err := readRegular(dir, filepath.Join("docs", "ok.md"), lstat(t, dir, "docs/ok.md"), 100); err != nil || string(b) != "fine\n" {
		t.Fatalf("readRegular(a regular file) = %q, %v", b, err)
	}
	os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "docs", "link.md"))
	os.Symlink("ok.md", filepath.Join(dir, "docs", "inside.md"))
	os.Symlink(outside, filepath.Join(dir, "away"))
	os.MkdirAll(filepath.Join(dir, "folder.md"), 0o755)
	for name, want := range map[string]string{
		"docs/link.md":   "symlink",
		"docs/inside.md": "symlink",
		"away/secret.md": "escapes",
		"folder.md":      "not a regular file",
	} {
		if b, err := readRegular(dir, filepath.FromSlash(name), lstat(t, dir, name), 100); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("readRegular(%s) = %q, %v; want a refusal mentioning %q", name, b, err, want)
		}
	}
	// A pipe is refused too, and opening it does not wait for a writer.
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readRegular(dir, "pipe.md", lstat(t, dir, "pipe.md"), 100); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("readRegular(a pipe) = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("reading a pipe waited for a writer")
	}
}

// A file over the limit is refused, never read cut short, and never read whole first.
func TestReadingAFileRefusesOneOverTheLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "exact.md"), []byte(strings.Repeat("x", 10)), 0o644)
	os.WriteFile(filepath.Join(dir, "big.md"), []byte(strings.Repeat("x", 11)), 0o644)
	if b, err := readRegular(dir, "exact.md", lstat(t, dir, "exact.md"), 10); err != nil || len(b) != 10 {
		t.Errorf("readRegular(a file at the limit) = %d bytes, %v", len(b), err)
	}
	if b, err := readRegular(dir, "big.md", lstat(t, dir, "big.md"), 10); err == nil || !strings.Contains(err.Error(), "more than 10 bytes") {
		t.Errorf("readRegular(a file over the limit) = %d bytes, %v", len(b), err)
	}
}
