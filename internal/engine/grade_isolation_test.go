package engine

import (
	"context"
	"github.com/ouroboric-dev/gleipnir/sandbox"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
	"github.com/ouroboric-dev/odin/internal/tree"
)

func TestGradingStartsOnlyAfterEverySandboxHasStopped(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	if _, err := g.r.SnapshotRunner(context.Background(), "move", g.r.Setup.GraphPack, nil); err != nil { // a snapshot sandbox, as an acceptance check starts
		t.Fatal(err)
	}
	var ups, downs, recorded int
	g.grader.Before = func(sandbox.GradeSpec) {
		ups, downs, recorded = len(g.box.Ups), len(g.box.Downs), len(g.r.State.Sandboxes)
	}
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	// The leaf's, the scratch agent's and the snapshot's: three, all down before the container starts.
	if ups != 3 || downs != 3 || recorded != 0 {
		t.Errorf("when the grader ran: %d sandboxes up in all, %d stopped, %d still recorded; want 3, 3, 0", ups, downs, recorded)
	}
}

func TestTheGraderGetsTheCommittedProjectAsAppAndNothingElse(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	os.WriteFile(filepath.Join(g.r.Project, "stray.txt"), []byte("not committed\n"), 0o644)
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	files := g.grader.Apps[0]
	for _, want := range []string{"umbrella.yaml", "repos/app/README.md"} {
		if !slices.Contains(files, want) {
			t.Errorf("/app lacks %s:\n%s", want, strings.Join(files, "\n"))
		}
	}
	for _, f := range files {
		for _, part := range strings.Split(f, "/") {
			if slices.Contains([]string{".git", ".factory", "stray.txt", "score.json", "build.txt"}, part) {
				t.Errorf("/app holds %s", f)
			}
		}
	}
}

func TestScoreJSONIsInNoLeafAndNoTree(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	path := filepath.Join(g.r.Dir, "score.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// No file called score.json is anywhere in a leaf's working copy, in what the grader was given as /app,
	// in a scratch workspace or in a snapshot; the only one is the run folder's.
	roots := []string{g.r.ScratchDir("design"), filepath.Join(g.r.Dir, "grade", "app")}
	for repo, n := range g.r.Setup.Repos {
		if repo != "." {
			roots = append(roots, n.Dir)
			if rel, err := filepath.Rel(n.Dir, path); err == nil && filepath.IsLocal(rel) {
				t.Errorf("score.json is inside the leaf %s", repo)
			}
		}
	}
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, _ error) error {
			if d != nil && !d.IsDir() && d.Name() == "score.json" {
				t.Errorf("a score.json at %s", p)
			}
			return nil
		})
	}
	// Neither the project umbrella nor a leaf tracks a score.json, or anything of the run folder.
	nodes, err := tree.Resolve(context.Background(), g.r.Setup.Root, filepath.Join(g.r.Setup.ProjectPath, "repos/app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []tree.Node{g.r.Setup.Project, nodes[len(nodes)-1]} {
		out, err := git.Repo{GitDir: n.GitDir, WorkTree: n.Dir}.Run(context.Background(), "ls-files", "--cached")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "score.json") || strings.Contains(out, ".factory") {
			t.Errorf("%s tracks or shows:\n%s", n.Dir, out)
		}
	}
}

// strs collects every string a value holds, through exported fields, slices, arrays, maps and pointers.
// It skips interfaces, which hold the readers and writers of a command.
func strs(v reflect.Value, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, v.String())
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			strs(v.Index(i), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			strs(k, out)
			strs(v.MapIndex(k), out)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			strs(v.Elem(), out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				strs(v.Field(i), out)
			}
		}
	}
}

// under names every string in v that is the folder or a path under it.
func under(v any, dir string) []string {
	var all, hit []string
	strs(reflect.ValueOf(v), &all)
	for _, s := range all {
		if s == dir || strings.HasPrefix(s, dir+"/") || strings.Contains(s, dir+"/") {
			hit = append(hit, s)
		}
	}
	return hit
}

func TestTheWalkerThatGuardsTheSuiteFolderFindsItWhereItIs(t *testing.T) {
	spec := gungnir.SandboxSpec{Run: "r", Name: "repos/app", Parent: "/p", Leaf: "/p/repos/app", GitDir: "/p/.git/modules/app"}
	if hit := under(spec, "/suites"); len(hit) != 0 {
		t.Errorf("found %v in a clean spec", hit)
	}
	spec.Leaf = "/suites/tiny-task/v1/tests"
	if hit := under(spec, "/suites"); len(hit) != 1 {
		t.Errorf("found %v in a spec that holds the suite", hit)
	}
	cmd := gungnir.Cmd{Argv: []string{"cat", "/suites/tiny-task/v1/tests/manifest.json"}, Env: map[string]string{"X": "/suites"}}
	if hit := under(cmd, "/suites"); len(hit) != 2 {
		t.Errorf("found %v in a command that reads the suite", hit)
	}
}

func TestNoAgentSandboxOrCommandEverHoldsTheSuiteFolder(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	if _, err := g.r.SnapshotRunner(context.Background(), "move", g.r.Setup.GraphPack, nil); err != nil {
		t.Fatal(err)
	}
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	suite := g.r.Setup.Config.Grader.SuiteDir
	if len(g.box.Ups) != 3 {
		t.Fatalf("%d sandboxes came up, want a leaf's, a scratch agent's and a snapshot's", len(g.box.Ups))
	}
	for _, up := range g.box.Ups {
		if hit := under(up, suite); len(hit) > 0 {
			t.Errorf("the sandbox %s holds the suite folder: %v", up.Name, hit)
		}
	}
	for _, c := range g.box.Execs {
		if hit := under(c, suite); len(hit) > 0 {
			t.Errorf("a command in a sandbox names the suite folder: %v", hit)
		}
	}
	// What an agent was told is no place to look either: its prompt and the files of its workspace.
	for _, key := range []string{"a-1", "design-1"} {
		if p := promptOf(t, g.r, key); strings.Contains(p, suite) || strings.Contains(p, "tiny-task/v1") {
			t.Errorf("the prompt of %s names the suite store", key)
		}
	}
	// The one container that holds the suite is the grader's, and it holds the tests folder only.
	if spec := g.grader.Specs[0]; spec.Tests != g.task.Tests || spec.App == suite || strings.HasPrefix(spec.App, suite) || strings.HasPrefix(spec.Logs, suite) {
		t.Errorf("the grader's folders = %+v", spec)
	}
}
