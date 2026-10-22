package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

func TestYggBoardShowsTheProjectsTasksByColumnAndAnEmptyProjectSaysSo(t *testing.T) {
	dir, _ := project(t, "script.yaml", "")
	code, out, errOut := ygg(t, "board", "--project", dir)
	if code != ExitDone || !strings.Contains(out, "no tasks") {
		t.Fatalf("empty project: exit %d:\n%s%s", code, out, errOut)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &schema.Features{Run: "20261023-090000-a1b2c3", Source: "tasks.md", SourceSHA256: strings.Repeat("a", 64), Tasks: []schema.FeatureRecord{
		{Feature: schema.Feature{ID: "g-T1", Title: "a task", Layer: "repos/api"}, Status: schema.TaskTodo}}}
	if err := f.Save(filepath.Join(dir, ".factory", "features.json")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = ygg(t, "board", "--project", dir)
	if code != ExitDone || !strings.Contains(out, "Ready (1)") || !strings.Contains(out, "g-T1") {
		t.Errorf("exit %d:\n%s%s", code, out, errOut)
	}
}
