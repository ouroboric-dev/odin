package run

import (
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func TestARunTakesALeafTheGraphDoesNotNameWhenAskedAndKeepsItOnResume(t *testing.T) {
	project := treetest.Project(t, "repos/app", "repos/extra")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	if _, ok := r.State.Leaves["repos/extra"]; ok {
		t.Fatal("a leaf no node names was started with the run")
	}
	if err := r.AddLeaf(ctx, "repos/extra"); err != nil {
		t.Fatalf("AddLeaf() = %v", err)
	}
	rec := r.State.Leaves["repos/extra"]
	if rec.Branch != Branch(id) || rec.Base == "" || rec.Base != rec.Checkpoint {
		t.Errorf("the leaf's record = %+v", rec)
	}
	if err := r.AddLeaf(ctx, "repos/extra"); err != nil {
		t.Errorf("a second AddLeaf() = %v; adding a leaf the run has is nothing", err)
	}
	l, _ := r.Leaf("repos/extra")
	if branch, _ := l.Git(ctx, "branch", "--show-current"); branch != Branch(id) {
		t.Errorf("repos/extra is on %q, want the run's branch", branch)
	}
	r.Close()
	r, err = Open(ctx, project, id, "")
	if err != nil {
		t.Fatalf("Open() = %v; a resume knows the leaves the run added", err)
	}
	defer r.Close()
	if _, ok := r.Setup.Repos["repos/extra"]; !ok || !strings.Contains(strings.Join(r.Setup.Leaves(), ","), "repos/extra") {
		t.Errorf("a resumed run's leaves = %v", r.Setup.Leaves())
	}
}

func TestAddLeafRefusesWhatIsNotALeafOfTheProject(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, p := range []string{"repos/nope", "../outside", "."} {
		if err := r.AddLeaf(ctx, p); err == nil {
			t.Errorf("AddLeaf(%q) = nil", p)
		}
	}
	if len(r.State.Leaves) != 1 {
		t.Errorf("leaves = %v", r.State.Leaves)
	}
}
