package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// outsideDir makes a folder beside the project and returns it.
func outsideDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "secret.md"), []byte("outside the project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPublishWritesNothingThroughASymlinkInTheProject(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	outside := outsideDir(t)
	if err := os.Symlink(outside, filepath.Join(r.Project, "docs")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "is a symlink") {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Errorf("the folder outside the project holds %d entries after a publish; it was written through the link", len(entries))
	}
	// A link deeper in the path too: docs is a folder, docs/1-product is not.
	os.Remove(filepath.Join(r.Project, "docs"))
	os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755)
	if err := os.Symlink(outside, filepath.Join(r.Project, "docs", "1-product")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("the folder outside the project holds %d entries; a link in the middle of the path was followed", len(entries))
	}
	if st, _ := git.Run(bg, r.Project, "status", "--porcelain", "--", "docs"); strings.Contains(st, "design.md") {
		t.Errorf("a document was staged: %q", st)
	}
}

// The plan is the planner's kept output, never a file in the project (TestThePlanIsTheOutputOdinKept); the leaves
// file the tasks node names, and the one init reads, are not read through a symlink.
func TestALeavesFileReachedThroughASymlinkIsNotRead(t *testing.T) {
	outside := outsideDir(t)
	os.WriteFile(filepath.Join(outside, "leaves.yaml"), []byte("leaves: {}\n"), 0o644)
	for _, node := range []string{planScratch + "  - { id: ready, kind: tasks, from: plan, leaves: leaves.yaml }\n", "  - { id: leaves, kind: init, from: leaves.yaml }\n"} {
		r2 := start(t, "key-points", "", node, "steps:\n  plan:\n    - result: { status: ok, output: '"+planDone+"' }\n")
		os.Symlink(filepath.Join(outside, "leaves.yaml"), filepath.Join(r2.Project, "leaves.yaml"))
		out, err := Advance(bg, r2)
		if err != nil || out.Status != run.StatusFailed || !(strings.Contains(out.Err.Error(), "symlink") || strings.Contains(out.Err.Error(), "not there as a regular file")) {
			t.Errorf("%s: %+v, %v", node, out, err)
		}
	}
}
