package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// A prompt that backticks an input its session does not declare is refused before the run starts (ruling R66).
func TestPrepareRefusesAPromptThatNamesAnInputItsSessionDoesNotDeclare(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	graph := loopRealm(t, map[string]func(string) string{"prompts/review-quality.md": func(s string) string {
		return s + "\nThe work is described by the `task`.\n"
	}})
	_, err := Prepare(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: graph})
	if err == nil || !strings.Contains(err.Error(), "task") || !strings.Contains(err.Error(), "review-quality.md") {
		t.Errorf("Prepare() = %v, want a refusal naming the prompt and the input task", err)
	}
}

func midgardToy(t *testing.T, implementer string) *realm.Realm {
	t.Helper()
	src, err := filepath.Abs("../../testdata/realms/midgard-toy")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "prompts", "implementer.md"), []byte(implementer), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := realm.Load(dst)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A loop with no prompt of its own takes its implementer's from the realm; that prompt may name only what the
// loop declares.
func TestARealmImplementerPromptMayNameOnlyWhatTheLoopDeclares(t *testing.T) {
	r := midgardToy(t, "Build the `task` you are given. Read the `brief` too.\n")
	g, err := schema.LoadGraph(filepath.Join(r.Dir, "graph.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	err = CheckRealmPrompts(g, []*realm.Realm{r})
	if err == nil || !strings.Contains(err.Error(), "brief") || !strings.Contains(err.Error(), "prompts/implementer.md") || !strings.Contains(err.Error(), "midgard-toy") {
		t.Fatalf("CheckRealmPrompts() = %v, want a refusal naming the realm, the prompt and brief", err)
	}
	r = midgardToy(t, "Build the `task` you are given, and answer `rejection` if any.\n")
	if err := CheckRealmPrompts(g, []*realm.Realm{r}); err != nil {
		t.Errorf("CheckRealmPrompts() = %v, want none: task and rejection are declared", err)
	}
}
