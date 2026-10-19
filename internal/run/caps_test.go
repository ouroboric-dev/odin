package run

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

var ctx = context.Background()

const (
	scriptedRoles = "roles:\n  planner: { agent: scripted, model: none, script: SCRIPT }\n  implementer: { agent: scripted, model: none, script: SCRIPT }\n"
	paidRoles     = "roles:\n  planner: { agent: opencode, model: openrouter/qwen }\n  implementer: { agent: scripted, model: none, script: SCRIPT }\n"
	fullCaps      = "budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
)

func toy(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/toy")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeConfig writes factory.yaml into a temporary directory, with SCRIPT replaced by the toy script.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "factory.yaml")
	body = strings.ReplaceAll(body, "SCRIPT", filepath.Join(toy(t), "script.yaml"))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunRefusesWithoutCompleteCaps(t *testing.T) {
	cases := []struct {
		name, config string
		missing      []string
	}{
		{"no caps", scriptedRoles, []string{"budget.per_run", "budget.per_day"}},
		{"a per-run cap but no per-day cap", scriptedRoles + "budget:\n  per_run: { tokens: 100000, turns: 100 }\n", []string{"budget.per_day"}},
		{"a paid-API role and no spend limit", paidRoles + fullCaps, []string{"budget.per_run.usd", "budget.per_day.usd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			project := treetest.Project(t, "repos/app")
			_, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), c.config), Graph: toy(t)})
			var ce *schema.CapsError
			if !errors.As(err, &ce) {
				t.Fatalf("Start() error = %v, want a *schema.CapsError", err)
			}
			for _, m := range c.missing {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("the error doesn't name %s:\n%s", m, err)
				}
			}
			if _, err := os.Stat(filepath.Join(project, ".factory")); !errors.Is(err, fs.ErrNotExist) {
				t.Error("Start() refused, but created .factory/")
			}
		})
	}
	t.Run("resume refuses in the same cases", func(t *testing.T) {
		project := treetest.Project(t, "repos/app")
		cfgDir := t.TempDir()
		r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, cfgDir, scriptedRoles+fullCaps), Graph: toy(t)})
		if err != nil {
			t.Fatal(err)
		}
		id := r.State.ID
		r.Close()
		writeConfig(t, cfgDir, scriptedRoles+"budget:\n  per_run: { tokens: 100000, turns: 100 }\n")
		_, err = Open(ctx, project, id, "")
		var ce *schema.CapsError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), "budget.per_day") {
			t.Errorf("Open() error = %v, want a *schema.CapsError naming budget.per_day", err)
		}
	})
	t.Run("complete caps start a run", func(t *testing.T) {
		project := treetest.Project(t, "repos/app")
		r, err := Start(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Graph: toy(t)})
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}
		defer r.Close()
		s, err := LoadState(r.Dir)
		if err != nil || s.Status != StatusRunning || s.Mode != gungnir.ModeKeyPoints || s.Ledger.Seq != 1 {
			t.Errorf("run.json = %+v, %v", s, err)
		}
	})
}
