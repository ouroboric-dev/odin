package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func TestOpenRefusesWhatWouldMisleadARun(t *testing.T) {
	start := func(t *testing.T) (project, id, realm string) {
		project, realm = t.TempDir(), t.TempDir()
		if err := os.CopyFS(realm, os.DirFS(toy(t))); err != nil {
			t.Fatal(err)
		}
		r, err := Start(Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: realm})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		return project, r.State.ID, realm
	}
	t.Run("a run another process holds", func(t *testing.T) {
		project, id, _ := start(t)
		r, err := Open(project, id, "")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if _, err := Open(project, id, ""); !errors.Is(err, ErrInUse) {
			t.Errorf("a second Open() = %v, want ErrInUse", err)
		}
	})
	t.Run("a graph edited after the run started", func(t *testing.T) {
		project, id, realm := start(t)
		g := filepath.Join(realm, "graph.yaml")
		b, _ := os.ReadFile(g)
		os.WriteFile(g, []byte(strings.Replace(string(b), "  - { id: dor,", "  - { id: lint, kind: exec, run: [echo] }\n  - { id: dor,", 1)), 0o644)
		if _, err := Open(project, id, ""); err == nil || !strings.Contains(err.Error(), "graph changed") {
			t.Errorf("Open() = %v, want a refusal because the graph changed", err)
		}
	})
	t.Run("a path as the run id", func(t *testing.T) {
		project, _, _ := start(t)
		if _, err := Open(project, "../../../tmp", ""); err == nil || !strings.Contains(err.Error(), "not a run id") {
			t.Errorf("Open() = %v, want not a run id", err)
		}
	})
	t.Run("a ledger cut short", func(t *testing.T) {
		project, id, _ := start(t)
		os.WriteFile(filepath.Join(Dir(project, id), "ledger.jsonl"), nil, 0o644)
		if _, err := Open(project, id, ""); err == nil || !strings.Contains(err.Error(), "lines were removed") {
			t.Errorf("Open() = %v, want lines were removed", err)
		}
	})
	t.Run("a looser mode is logged", func(t *testing.T) {
		project, id, _ := start(t)
		r, err := Open(project, id, gungnir.ModeUnattended)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		b, _ := os.ReadFile(filepath.Join(Dir(project, id), "ledger.jsonl"))
		if !strings.Contains(string(b), `"event":"mode_changed"`) || !strings.Contains(string(b), `"ygg.mode.looser":true`) {
			t.Errorf("the ledger doesn't log the looser mode:\n%s", b)
		}
	})
}
