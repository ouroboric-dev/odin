package run

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// A hash already kept stays: it is what the run started with, and other bytes are refused (ruling R56).
	r.State.PromptSHA256["prompts/build.md"] = strings.Repeat("0", 64)
	var ce *ChangedError
	if _, err := r.ReadPrompt("", "prompts/build.md"); !errors.As(err, &ce) || r.State.PromptSHA256["prompts/build.md"] != strings.Repeat("0", 64) {
		t.Errorf("ReadPrompt() = %v, and the kept hash is now %v", err, r.State.PromptSHA256)
	}
	for _, bad := range []string{"../graph.yaml", "/etc/passwd", "prompts/../../x.md"} {
		if _, err := r.ReadPrompt("", bad); err == nil || !strings.Contains(err.Error(), "not inside") {
			t.Errorf("ReadPrompt(%q) = %v, want a refusal", bad, err)
		}
	}
}

// A prompt whose bytes are not the ones the run kept is refused with what changed (ruling R56); a realm's prompt,
// kept the first time it is read, is checked on every later read.
func TestAPromptThatChangedDuringTheRunIsRefused(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	pack := t.TempDir()
	if err := os.CopyFS(pack, os.DirFS(toy(t))); err != nil {
		t.Fatal(err)
	}
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: pack})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before, _ := os.ReadFile(filepath.Join(pack, "prompts", "plan.md"))
	if err := os.WriteFile(filepath.Join(pack, "prompts", "plan.md"), []byte("Another plan prompt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = r.ReadPrompt("", "prompts/plan.md")
	var ce *ChangedError
	want := "the prompt prompts/plan.md changed during the run (recorded sha256 " + sha(before)[:12] + "…, now " + sha([]byte("Another plan prompt.\n"))[:12] + "…); start a new run"
	if !errors.As(err, &ce) || err.Error() != want {
		t.Errorf("ReadPrompt() = %v, want %q", err, want)
	}
	// A prompt first read later (as a realm's is) is kept then, and checked from then on.
	delete(r.State.PromptSHA256, "prompts/build.md")
	if _, err := r.ReadPrompt("", "prompts/build.md"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "prompts", "build.md"), []byte("Another build prompt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadPrompt("", "prompts/build.md"); !errors.As(err, &ce) {
		t.Errorf("ReadPrompt() of a changed prompt kept on its first read = %v, want a ChangedError", err)
	}
}

// A private skill whose files changed during the run is refused when a session loads it (ruling R56).
func TestASkillThatChangedDuringTheRunIsRefused(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	private := t.TempDir()
	skill := filepath.Join(private, "house-style", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: house-style\ndescription: How we write.\n---\nUse short words.\n"
	if err := os.WriteFile(skill, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps+"skills:\n  private_dir: "+private+"\n")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Graph: realmWithSkills(t, "house-style")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	text, err := r.LoadSkills([]string{"house-style"})
	if err != nil || !strings.Contains(text, "Use short words.") {
		t.Fatalf("LoadSkills() = %q, %v", text, err)
	}
	if err := os.WriteFile(skill, []byte(strings.Replace(body, "short", "long", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadSkills([]string{"house-style"})
	var ce *ChangedError
	if !errors.As(err, &ce) || !strings.HasPrefix(err.Error(), "the skill house-style changed during the run (recorded sha256 ") ||
		!strings.HasSuffix(err.Error(), "…); start a new run") {
		t.Errorf("LoadSkills() of a changed skill = %v", err)
	}
}
