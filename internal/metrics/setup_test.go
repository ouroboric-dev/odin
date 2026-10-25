package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// setupFake is a run whose run.json keeps the hashes of its prompts and skills, with a factory.yaml that sets a
// role's reasoning and a session that names its CLI's version. Its graph pack's folder does not exist: the record
// reads the hashes the run kept, never the files.
func setupFake(t *testing.T) *fake {
	t.Helper()
	config := filepath.Join(t.TempDir(), "factory.yaml")
	body := "roles:\n  planner: { agent: scripted, model: none, script: s.yaml, reasoning: low }\n" +
		"  implementer: { agent: scripted, model: none, script: s.yaml }\n" +
		"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	st := f.State
	st.Graph, st.GraphDir, st.GraphPackSHA256 = "toy", filepath.Join(t.TempDir(), "gone"), strings.Repeat("9", 64)
	st.ConfigPath, st.ConfigSHA256 = config, sum([]byte(body))
	st.Factory = "v0.9.0-test"
	st.Roles = map[string]run.RoleRecord{"planner": {Agent: "scripted", Model: "none"}, "implementer": {Agent: "scripted", Model: "none"}}
	st.Realms = map[string]run.RealmRecord{"helheim-go": {Version: "0.1.0", Dir: "/nowhere", SHA256: strings.Repeat("ab", 32)}}
	st.PromptSHA256 = map[string]string{"prompts/design.md": strings.Repeat("1", 64), "helheim-go:prompts/implementer.md": strings.Repeat("2", 64)}
	st.SkillSHA256 = map[string]string{"ponytail-review": strings.Repeat("3", 64), "house-style": strings.Repeat("cd", 32)}
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted, FactoryVersion: "v0.9.0-test"})
	ev := session("build", "implementer", "scripted", "none", 10, 1, 1, 0, false)
	ev.CLIVersion = "2.1.0 (Claude Code)"
	f.log(1, ev)
	return f
}

func TestTheRecordHoldsTheSetup(t *testing.T) {
	f := setupFake(t)
	r := mustCollect(t, f.done())
	s := r.Setup
	if got := s.Roles["planner"]; got != (RoleSetup{Agent: "scripted", Model: "none", Reasoning: "low"}) {
		t.Errorf("roles[planner] = %+v", got)
	}
	if got := s.Roles["implementer"]; got != (RoleSetup{Agent: "scripted", Model: "none", CLIVersion: "2.1.0 (Claude Code)"}) {
		t.Errorf("roles[implementer] = %+v", got)
	}
	if s.Graph["toy"] != f.State.GraphPackSHA256 || len(s.Graph) != 1 {
		t.Errorf("graph = %v, want the pack's hash", s.Graph)
	}
	if !maps.Equal(s.Prompts, f.State.PromptSHA256) {
		t.Errorf("prompts = %v, want the hashes run.json keeps, %v", s.Prompts, f.State.PromptSHA256)
	}
	if !maps.Equal(s.Skills, f.State.SkillSHA256) {
		t.Errorf("skills = %v, want the hashes run.json keeps, %v", s.Skills, f.State.SkillSHA256)
	}
	if s.Realms["helheim-go"] != strings.Repeat("ab", 32) {
		t.Errorf("realms = %v", s.Realms)
	}
	if s.Factory != "v0.9.0-test" {
		t.Errorf("factory = %q", s.Factory)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v", r.Warnings)
	}

	// A prompt's hash changes in run.json, as a run on another prompt records it: the record follows.
	f.State.PromptSHA256["prompts/design.md"] = strings.Repeat("4", 64)
	if got := mustCollect(t, f.done()).Setup.Prompts["prompts/design.md"]; got != strings.Repeat("4", 64) {
		t.Errorf("prompts/design.md = %s after its hash changed", got)
	}
}

// A run from before run.json kept the hashes of its prompts and skills has none in its record, which says why.
func TestARunThatKeptNoPromptHashesSaysSo(t *testing.T) {
	f := setupFake(t)
	f.State.PromptSHA256, f.State.SkillSHA256 = nil, nil
	r := mustCollect(t, f.done())
	if len(r.Setup.Prompts) != 0 || len(r.Setup.Skills) != 0 {
		t.Errorf("prompts %v, skills %v; want none", r.Setup.Prompts, r.Setup.Skills)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "keeps no hash of its prompts") {
		t.Errorf("warnings = %v", r.Warnings)
	}
}
