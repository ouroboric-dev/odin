package tracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func writeList(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheOperatorsProjectsAreReadFromTheLocalListInOrder(t *testing.T) {
	a, b := treetest.Project(t, "repos/app"), treetest.Project(t, "repos/app")
	got, err := LoadProjects(writeList(t, "projects:\n  - "+b+"\n  - "+a+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Errorf("LoadProjects() = %v, want [%s %s]", got, b, a)
	}
}

// Change project-is-its-own-tree-T002: a path that is not a project umbrella is refused, and nothing is read.
func TestAListNamingAnythingButProjectRootsIsRefusedWholeWithTheBadPath(t *testing.T) {
	good := treetest.Project(t, "repos/app")
	notRoot := t.TempDir()
	umbrella, _ := os.ReadFile(filepath.Join(good, "umbrella.yaml"))
	if err := os.WriteFile(filepath.Join(notRoot, "umbrella.yaml"), []byte(strings.Replace(string(umbrella), "root: true", "root: false", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(good, "repos", "app")
	for name, body := range map[string]string{
		"a leaf folder":         "projects:\n  - " + good + "\n  - " + leaf + "\n",
		"an umbrella below one": "projects:\n  - " + notRoot + "\n",
		"a folder with nothing": "projects:\n  - " + t.TempDir() + "\n",
		"a folder that is gone": "projects:\n  - /no/such/project\n",
		"a relative path":       "projects:\n  - projects/gallery\n",
		"a home shortcut":       "projects:\n  - ~/gallery\n",
		"the same one twice":    "projects:\n  - " + good + "\n  - " + good + "/\n",
		"an empty list":         "projects: []\n",
		"an unknown key":        "projects:\n  - " + good + "\nextra: 1\n",
	} {
		if got, err := LoadProjects(writeList(t, body)); err == nil || got != nil {
			t.Errorf("%s: LoadProjects() = %v, %v; want a refusal and no projects", name, got, err)
		}
	}
	_, err := LoadProjects(writeList(t, "projects:\n  - "+good+"\n  - "+leaf+"\n"))
	if err == nil || !strings.Contains(err.Error(), leaf) {
		t.Errorf("the refusal must name the bad path %s: %v", leaf, err)
	}
}

func TestTheDefaultListIsInTheOperatorsConfigFolder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got, err := DefaultProjectsFile(); err != nil || got != "/cfg/ouroboric/projects.yaml" {
		t.Errorf("DefaultProjectsFile() = %q, %v", got, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got, err := DefaultProjectsFile(); err != nil || got != "/home/someone/.config/ouroboric/projects.yaml" {
		t.Errorf("DefaultProjectsFile() = %q, %v", got, err)
	}
	// The XDG spec says a relative XDG_CONFIG_HOME is invalid and must be ignored.
	t.Setenv("XDG_CONFIG_HOME", "relative/cfg")
	if got, err := DefaultProjectsFile(); err != nil || got != "/home/someone/.config/ouroboric/projects.yaml" {
		t.Errorf("a relative XDG_CONFIG_HOME: DefaultProjectsFile() = %q, %v; want the HOME default", got, err)
	}
}

func TestTheDefaultListIsRefusedWithoutAnAbsoluteHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, home := range []string{"", "relative/home"} {
		t.Setenv("HOME", home)
		if got, err := DefaultProjectsFile(); err == nil || got != "" {
			t.Errorf("HOME=%q: DefaultProjectsFile() = %q, %v; want a refusal that names the problem", home, got, err)
		}
	}
}

// A control character in a folder name is written in the refusal as a Go quote, never raw to the terminal.
func TestAPathWithAControlCharacterIsQuotedInTheRefusal(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "gallery\x1b[31m")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// In the YAML list the escape is written as \e, which the parser reads as the control character.
	_, err := LoadProjects(writeList(t, "projects:\n  - \""+strings.Replace(bad, "\x1b", `\e`, 1)+"\"\n"))
	if err == nil || strings.ContainsRune(err.Error(), '\x1b') || !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("LoadProjects() error %q; want the path quoted, with no raw control character", err)
	}
}

func TestOneBoardPerProjectIsWrittenWithTheProjectAsItsHeading(t *testing.T) {
	var w strings.Builder
	WriteAll(&w, []gungnir.Board{{Project: "/p/one", Cards: []gungnir.Card{{Task: "a-T1", Column: gungnir.ColumnDone}}}, {Project: "/p/two"}})
	out := w.String()
	if !strings.Contains(out, "/p/one (1 done)") || !strings.Contains(out, "/p/two (no tasks)") || strings.Index(out, "/p/one") > strings.Index(out, "/p/two") {
		t.Errorf("WriteAll() =\n%s", out)
	}
}
