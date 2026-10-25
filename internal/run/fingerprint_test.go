package run

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A run keeps in run.json, when it starts, the hash of every prompt its graph pack gives a session and of every
// skill it may load: the record's setup is what the run used, whatever happens to the files later.
func TestRunJSONKeepsTheHashOfEachPromptAndSkill(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	pack := realmWithSkills(t, "plain-language")
	libs := skillLib(t)
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: pack, Hooks: Hooks{Libs: libs}})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close()
	s, err := LoadState(Dir(project, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"prompts/design.md", "prompts/spec.md", "prompts/plan.md", "prompts/build.md"} {
		b, err := os.ReadFile(filepath.Join(pack, p))
		if err != nil {
			t.Fatal(err)
		}
		if s.PromptSHA256[p] != sha(b) {
			t.Errorf("prompt_sha256[%s] = %q, want %s", p, s.PromptSHA256[p], sha(b))
		}
	}
	if len(s.PromptSHA256) != 4 {
		t.Errorf("prompt_sha256 = %v, want the graph's four prompts", s.PromptSHA256)
	}
	loaded, err := havamal.Select([]string{"plain-language"}, libs.Pinned, libs.Private)
	if err != nil {
		t.Fatal(err)
	}
	if want := havamal.SetHash([]havamal.Entry{loaded[0].Entry}); s.SkillSHA256["plain-language"] != want || len(s.SkillSHA256) != 1 {
		t.Errorf("skill_sha256 = %v, want plain-language at %s", s.SkillSHA256, want)
	}
}

// A session's prompt is read through the run, which keeps the hash of a prompt it had not hashed yet (a realm's,
// known only when a task's loop expands) and reads nothing outside the pack or realm.
func TestAPromptIsReadInsideItsPackAndItsHashKept(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: toy(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	delete(r.State.PromptSHA256, "prompts/build.md")
	b, err := r.ReadPrompt("", "prompts/build.md")
	if err != nil || r.State.PromptSHA256["prompts/build.md"] != sha(b) {
		t.Errorf("ReadPrompt() = %v; prompt_sha256 = %v", err, r.State.PromptSHA256)
	}
	// A hash already kept stays: it is what the run started with.
	r.State.PromptSHA256["prompts/build.md"] = strings.Repeat("0", 64)
	if _, err := r.ReadPrompt("", "prompts/build.md"); err != nil || r.State.PromptSHA256["prompts/build.md"] != strings.Repeat("0", 64) {
		t.Errorf("ReadPrompt() = %v, and replaced the kept hash: %v", err, r.State.PromptSHA256)
	}
	for _, bad := range []string{"../graph.yaml", "/etc/passwd", "prompts/../../x.md"} {
		if _, err := r.ReadPrompt("", bad); err == nil || !strings.Contains(err.Error(), "not inside") {
			t.Errorf("ReadPrompt(%q) = %v, want a refusal", bad, err)
		}
	}
}
