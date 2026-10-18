package deps

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSnapshotRestoresChangedRemovedAndCreatedFilesByteForByte(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module m\r\n\x00binary\xff")
	write(t, dir, "go.sum", "sum\n")
	write(t, dir, "src/a.go", "package a\n")
	write(t, dir, "devcontainer/checks/config/deps-policy.json", `{"schema":"ygg.deps/v1"}`)
	write(t, dir, "node_modules/x/index.js", "cache")
	write(t, dir, ".git", "gitdir: ../x")
	if err := os.Chmod(filepath.Join(dir, "src/a.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/a.go", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	snap, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := snap.Changes(); err != nil || len(ch) != 0 {
		t.Fatalf("changes of an untouched product: %v, %v", ch, err)
	}
	write(t, dir, "go.mod", "changed")
	os.Remove(filepath.Join(dir, "go.sum"))
	write(t, dir, "package-lock.json", "{}")
	write(t, dir, "new/deep/file", "x")
	write(t, dir, "devcontainer/checks/config/deps-policy.json", `{"schema":"weakened"}`)
	os.Chmod(filepath.Join(dir, "src/a.go"), 0o600)
	os.Remove(filepath.Join(dir, "link"))
	write(t, dir, "node_modules/x/index.js", "changed cache, not counted")
	want := []string{"devcontainer/checks/config/deps-policy.json", "go.mod", "go.sum", "link", "new/deep/file", "package-lock.json", "src/a.go"}
	if ch, err := snap.Changes(); err != nil || !reflect.DeepEqual(ch, want) {
		t.Fatalf("changes = %v, %v; want %v", ch, err, want)
	}
	if err := snap.Restore(); err != nil {
		t.Fatal(err)
	}
	if ch, _ := snap.Changes(); len(ch) != 0 {
		t.Errorf("still changed after restore: %v", ch)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(b) != "module m\r\n\x00binary\xff" {
		t.Errorf("go.mod = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Errorf("the folder the tool created is still there: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "link")); err != nil || l != "src/a.go" {
		t.Errorf("symlink = %q, %v", l, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "src/a.go")); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode())
	}
}

func TestSnapshotSkipsOnlyTheTopLevelGitAndRestoresANestedOne(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".git/HEAD", "ref")
	write(t, dir, "sub/a", "a")
	snap, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, ".git/HEAD", "moved") // the leaf's own .git is not the snapshot's
	write(t, dir, "sub/.git/config", "[core]\nhooksPath = evil")
	ch, err := snap.Changes()
	if err != nil || len(ch) != 1 || ch[0] != "sub/.git/config" {
		t.Fatalf("changes = %v, %v", ch, err)
	}
	if err := snap.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub/.git")); !os.IsNotExist(err) {
		t.Errorf("the nested .git is still there: %v", err)
	}
}

func TestSnapshotRefusesAProductTooBigToUndo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "f", "x")
	if _, err := takeLimited(dir, 0); err == nil { // one byte is over a limit of none
		t.Error("a snapshot over the limit was taken")
	}
}
