package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/run"
)

// copyDir copies the folder src into a new temporary folder and returns it.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// setupFake is a run of the toy graph from a copy of its pack, with a factory.yaml that sets a role's reasoning,
// a pinned skill and a private one, and a session that names its CLI's version.
func setupFake(t *testing.T, pack string) *fake {
	t.Helper()
	config := filepath.Join(t.TempDir(), "factory.yaml")
	body := "roles:\n  planner: { agent: scripted, model: none, script: s.yaml, reasoning: low }\n" +
		"  implementer: { agent: scripted, model: none, script: s.yaml }\n" +
		"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := havamal.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	pinned := lib.Names()[0]
	s, err := lib.Get(pinned)
	if err != nil {
		t.Fatal(err)
	}
	packHash, err := realm.Hash(pack)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	st := f.State
	st.Graph, st.GraphDir, st.GraphPackSHA256 = "toy", pack, packHash
	st.ConfigPath, st.ConfigSHA256 = config, sum([]byte(body))
	st.Factory = "v0.9.0-test"
	st.Roles = map[string]run.RoleRecord{"planner": {Agent: "scripted", Model: "none"}, "implementer": {Agent: "scripted", Model: "none"}}
	st.Realms = map[string]run.RealmRecord{"helheim-go": {Version: "0.1.0", Dir: "/nowhere", SHA256: strings.Repeat("ab", 32)}}
	st.Skills = skills.Record{Hash: havamal.SetHash([]havamal.Entry{s.Entry}), Skills: []string{"house-style", pinned},
		Private: []skills.PrivateRecord{{Name: "house-style", SHA256: strings.Repeat("cd", 32)}}}
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted, FactoryVersion: "v0.9.0-test"})
	ev := session("build", "implementer", "scripted", "none", 10, 1, 1, 0, false)
	ev.CLIVersion = "2.1.0 (Claude Code)"
	f.log(1, ev)
	return f
}

func TestTheRecordHoldsTheSetup(t *testing.T) {
	pack := copyDir(t, "../../testdata/toy")
	f := setupFake(t, pack)
	first := mustCollect(t, f.done())
	s := first.Setup
	if got := s.Roles["planner"]; got != (RoleSetup{Agent: "scripted", Model: "none", Reasoning: "low"}) {
		t.Errorf("roles[planner] = %+v", got)
	}
	if got := s.Roles["implementer"]; got != (RoleSetup{Agent: "scripted", Model: "none", CLIVersion: "2.1.0 (Claude Code)"}) {
		t.Errorf("roles[implementer] = %+v", got)
	}
	if s.Graph["toy"] != f.State.GraphPackSHA256 || len(s.Graph) != 1 {
		t.Errorf("graph = %v, want the pack's hash", s.Graph)
	}
	design, err := os.ReadFile(filepath.Join(pack, "prompts", "design.md"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Prompts["prompts/design.md"] != sum(design) || s.Prompts["prompts/build.md"] == "" {
		t.Errorf("prompts = %v, want every prompt the graph names, by its path", s.Prompts)
	}
	if s.Realms["helheim-go"] != strings.Repeat("ab", 32) {
		t.Errorf("realms = %v", s.Realms)
	}
	if len(s.Skills) != 2 || s.Skills["house-style"] != strings.Repeat("cd", 32) {
		t.Errorf("skills = %v, want the pinned skill and the private one", s.Skills)
	}
	if s.Factory != "v0.9.0-test" {
		t.Errorf("factory = %q", s.Factory)
	}
	if len(first.Warnings) != 0 {
		t.Errorf("warnings = %v", first.Warnings)
	}

	// The prompt changes, and a new run records the pack's new hash: only that prompt's hash, and the graph's, change.
	if err := os.WriteFile(filepath.Join(pack, "prompts", "design.md"), append(design, "One more line.\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if f.State.GraphPackSHA256, err = realm.Hash(pack); err != nil {
		t.Fatal(err)
	}
	second := mustCollect(t, f.done()).Setup
	if second.Prompts["prompts/design.md"] == s.Prompts["prompts/design.md"] {
		t.Error("the design prompt changed and its hash did not")
	}
	if second.Prompts["prompts/build.md"] != s.Prompts["prompts/build.md"] {
		t.Error("the build prompt did not change and its hash did")
	}
	if second.Graph["toy"] == s.Graph["toy"] {
		t.Error("a prompt changed and the graph's hash did not")
	}
}

// A graph pack whose files changed since the run gives no prompt hashes: they would be of other prompts than the
// run's. The record says why.
func TestARecordOfAChangedPackHasNoPromptHashes(t *testing.T) {
	pack := copyDir(t, "../../testdata/toy")
	f := setupFake(t, pack)
	if err := os.WriteFile(filepath.Join(pack, "prompts", "design.md"), []byte("Another prompt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := mustCollect(t, f.done())
	if len(r.Setup.Prompts) != 0 {
		t.Errorf("prompts = %v, want none", r.Setup.Prompts)
	}
	if r.Setup.Graph["toy"] != f.State.GraphPackSHA256 {
		t.Errorf("graph = %v, want the hash the run recorded", r.Setup.Graph)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "changed since the run") {
		t.Errorf("warnings = %v", r.Warnings)
	}
}
