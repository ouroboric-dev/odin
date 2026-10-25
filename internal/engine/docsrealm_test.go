package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

// Backlog B-036: a realm whose output is documents has no code leaves, no stack layer and no leaf recipe
// (the Asgard realm is the first). A run of it works in a project umbrella with no leaves.
func TestARealmWithNoLeavesNoStackAndNoRecipeRunsInAnUmbrellaWithNoLeaves(t *testing.T) {
	r := startRealms(t, realmsSetup{mode: "unattended", nodes: publishNodes, script: publishScript,
		roles:  "gates:\n  design: auto\n" + defaultRoles,
		realms: []realmFiles{{name: "asgard", version: "0.1.0", files: map[string]string{"p.md": "Write the charter.\n"}}}})
	if len(r.Setup.Leaves()) != 0 || len(r.State.Leaves) != 0 || r.Setup.GraphPack.Pack.Leaf != nil || r.Setup.GraphPack.Pack.Sandbox != nil {
		t.Fatalf("leaves %v, recipe %v, sandbox %v", r.Setup.Leaves(), r.Setup.GraphPack.Pack.Leaf, r.Setup.GraphPack.Pack.Sandbox)
	}
	onDay(r)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(r.Project, filepath.FromSlash(designMD))); err != nil || !strings.Contains(string(b), "## Summary\n\nAlbums of photos.\n") {
		t.Errorf("the document was not published: %q, %v", b, err)
	}
	if st, _ := loadStatus(t, r); !strings.Contains(st, "done") {
		t.Errorf("status = %s", st)
	}
}

func loadStatus(t *testing.T, r *run.Run) (string, error) {
	t.Helper()
	s, err := run.LoadState(r.Dir)
	if err != nil {
		return "", err
	}
	return string(s.Status), nil
}
