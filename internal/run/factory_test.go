package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/tree/treetest"
	"github.com/ouroboric-dev/odin/internal/version"
)

func TestARunRecordsTheBuildOfYggThatStartedAndResumedIt(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	realm := t.TempDir()
	if err := os.CopyFS(realm, os.DirFS(toy(t))); err != nil {
		t.Fatal(err)
	}
	r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: realm})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	want := version.Factory()
	if r.State.Factory != want {
		t.Errorf("State.Factory = %q, want %q", r.State.Factory, want)
	}
	again, err := Open(ctx, project, r.State.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Resumed("a test"); err != nil {
		t.Fatal(err)
	}
	again.Close()
	b, _ := os.ReadFile(filepath.Join(Dir(project, r.State.ID), "ledger.jsonl"))
	line := `"ygg.factory.version":"` + want + `"`
	if n := strings.Count(string(b), line); n != 2 {
		t.Errorf("the ledger names the build %d times, want 2 (run_started and run_resumed):\n%s", n, b)
	}
	state, _ := os.ReadFile(filepath.Join(Dir(project, r.State.ID), "run.json"))
	if !strings.Contains(string(state), `"factory": "`+want+`"`) {
		t.Errorf("run.json does not record the build:\n%s", state)
	}
}
