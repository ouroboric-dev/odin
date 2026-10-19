package run

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

type noGrader struct{}

func (noGrader) Grade(context.Context, sandbox.GradeSpec) (sandbox.GradeResult, error) {
	return sandbox.GradeResult{}, nil
}

// graderBlock is a grader section for the task tk.
func graderBlock(tk *tasktest.Task) string {
	return "grader:\n  tasks_dir: " + tk.TasksDir + "\n  suite_dir: " + tk.SuiteDir + "\n  timeout: 30m\n  memory: 4g\n  cpus: 4\n"
}

// gradeRealm is the toy sandbox realm with a grade node added to the end of its graph.
func gradeRealm(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "toy")
	if err := os.CopyFS(dir, os.DirFS(toySandbox(t))); err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(filepath.Join(dir, "graph.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "graph.yaml"), append(g, []byte("  - { id: score, kind: grade }\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAGradeNodeNeedsTheGraderAndTheSandboxSections(t *testing.T) {
	tk := tasktest.New(t)
	script := filepath.Join(toySandbox(t), "script.yaml")
	head := "roles:\n  planner: { agent: scripted, model: none, script: " + script + " }\n  implementer: { agent: scripted, model: none, script: " + script + " }\n" + fullCaps
	for name, body := range map[string]string{
		"no grader section":  head + sandboxSection,
		"no sandbox section": head + graderBlock(tk),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "factory.yaml")
			os.WriteFile(cfg, []byte(body), 0o644)
			_, err := Prepare(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: cfg, Graph: gradeRealm(t)})
			if err == nil || !strings.Contains(err.Error(), "node score") || !strings.Contains(err.Error(), "grader and the sandbox sections") {
				t.Errorf("Prepare() = %v, want a refusal that names the node and the sections", err)
			}
		})
	}
}

func TestTheRunHoldsAGraderOnlyWithBothSections(t *testing.T) {
	tk := tasktest.New(t)
	for name, c := range map[string]struct {
		extra string
		want  bool
	}{"with the grader section": {graderBlock(tk), true}, "without": {"", false}} {
		t.Run(name, func(t *testing.T) {
			made := 0
			h := Hooks{
				NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return &sandboxfake.Sandbox{}, nil },
				NewGrader:  func(*SandboxSetup, string) (Grader, error) { made++; return noGrader{}, nil },
			}
			realm := toySandbox(t)
			r, err := Start(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: sandboxConfig(t, "{ agent: scripted, model: none, script: "+filepath.Join(realm, "script.yaml")+" }", c.extra), Graph: realm, Hooks: h})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if (r.Grader != nil) != c.want || (made == 1) != c.want {
				t.Errorf("grader %v, made %d; want a grader: %v", r.Grader, made, c.want)
			}
		})
	}
}

func TestExportAppIsTheProjectAsCommittedAndNothingMore(t *testing.T) {
	realm := toySandbox(t)
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return &sandboxfake.Sandbox{}, nil }}
	r, err := Start(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: sandboxConfig(t, "{ agent: scripted, model: none, script: "+filepath.Join(realm, "script.yaml")+" }", ""), Graph: realm, Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.WriteFile(filepath.Join(r.Project, "stray.txt"), []byte("not committed\n"), 0o644)
	dst := filepath.Join(r.GradeDir(), "app")
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(dst, "left-from-before.txt"), []byte("old\n"), 0o644)
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("ExportApp() = %v", err)
	}
	var files []string
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, _ error) error {
		if rel, _ := filepath.Rel(dst, p); !d.IsDir() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	for _, want := range []string{"umbrella.yaml", "repos/app/README.md"} {
		if !slices.Contains(files, want) {
			t.Errorf("the export lacks %s: %v", want, files)
		}
	}
	for _, f := range files {
		for _, part := range strings.Split(f, "/") {
			if slices.Contains([]string{".git", ".factory", "stray.txt", "left-from-before.txt"}, part) {
				t.Errorf("the export holds %s", f)
			}
		}
	}
}

func TestNoSandboxesLeftRefusesWhileOneIsRecordedAsUp(t *testing.T) {
	r := &Run{State: &State{}}
	if err := r.NoSandboxesLeft(); err != nil {
		t.Errorf("NoSandboxesLeft() = %v on a run with none", err)
	}
	r.State.Sandboxes = map[string]gungnir.Env{"repos/app": {Name: "repos/app"}}
	if err := r.NoSandboxesLeft(); err == nil || !strings.Contains(err.Error(), "still up") {
		t.Errorf("NoSandboxesLeft() = %v, want a refusal", err)
	}
}
