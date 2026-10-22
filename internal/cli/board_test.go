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

func TestYggBoardAllShowsEveryProjectOfTheLocalListAndRefusesAListWithAnImpostor(t *testing.T) {
	a, _ := project(t, "script.yaml", "")
	b, _ := project(t, "script.yaml", "")
	list := filepath.Join(t.TempDir(), "projects.yaml")
	if err := os.WriteFile(list, []byte("projects:\n  - "+a+"\n  - "+b+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "board", "--all", "--projects", list)
	if code != ExitDone || !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if err := os.WriteFile(list, []byte("projects:\n  - "+a+"\n  - "+t.TempDir()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := ygg(t, "board", "--all", "--projects", list); code != ExitRefused || out != "" || errOut == "" {
		t.Errorf("a list with a folder that is no project: exit %d, stdout %q, stderr %q; want a refusal and no board", code, out, errOut)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if code, _, errOut := ygg(t, "board", "--all"); code != ExitRefused || !strings.Contains(errOut, "projects.yaml") {
		t.Errorf("no list: exit %d, stderr %q; want a refusal that says where the list goes", code, errOut)
	}
	if code, _, _ := ygg(t, "board", "--all", "--project", a); code != ExitRefused {
		t.Errorf("--all with --project: exit %d, want a refusal", code)
	}
}

// One board that cannot be read refuses the whole list: the healthy board before it is not shown.
func TestYggBoardAllRefusesTheWholeListWhenOneProjectsBoardFails(t *testing.T) {
	a, _ := project(t, "script.yaml", "")
	b, _ := project(t, "script.yaml", "")
	if err := os.MkdirAll(filepath.Join(b, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, ".factory", "features.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(t.TempDir(), "projects.yaml")
	if err := os.WriteFile(list, []byte("projects:\n  - "+a+"\n  - "+b+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "board", "--all", "--projects", list)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, b) {
		t.Errorf("a corrupt features.json in the second project: exit %d, stdout %q, stderr %q; want exit %d, no board, and the stderr to name %s", code, out, errOut, ExitRefused, b)
	}
}

// A folder name with a control character is quoted in the refusal, never printed raw to the terminal.
func TestYggBoardAllQuotesAControlCharacterInAPath(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "gallery\x1b[31m")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(t.TempDir(), "projects.yaml")
	// The YAML escape \e is the control character once the list is parsed.
	if err := os.WriteFile(list, []byte("projects:\n  - \""+strings.Replace(bad, "\x1b", `\e`, 1)+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "board", "--all", "--projects", list)
	if code != ExitRefused || out != "" || strings.ContainsRune(errOut, '\x1b') || !strings.Contains(errOut, `\x1b`) {
		t.Errorf("exit %d, stdout %q, stderr %q; want a refusal with the path quoted and no raw control character", code, out, errOut)
	}
}

func TestYggBoardRefusesProjectsWithoutAll(t *testing.T) {
	dir, _ := project(t, "script.yaml", "")
	code, out, errOut := ygg(t, "board", "--project", dir, "--projects", filepath.Join(t.TempDir(), "projects.yaml"))
	if code != ExitRefused || !strings.Contains(errOut, "--projects") || !strings.Contains(errOut, "--all") {
		t.Errorf("exit %d, want %d and a message naming --projects and --all:\n%s%s", code, ExitRefused, out, errOut)
	}
}
