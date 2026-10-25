package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/task"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// fakeGrader stands in for Gleipnir's driver. It prints Results on its stdout as the hidden suite's test.sh
// would, on a line with the key it is given, and lists the exported /app as it is at that moment.
type fakeGrader struct {
	mu      sync.Mutex
	Specs   []sandbox.GradeSpec
	Apps    [][]string              // the files of /app, for each call
	Results string                  // the report to print on a keyed line, among other output; "" prints none
	Stdout  func(key string) string // what to print instead, whole, when it is set
	Forged  string                  // a results.json to write to the logs folder, as an app that wrote one would
	Err     error                   // what Grade returns
	Before  func(sandbox.GradeSpec)
}

func (f *fakeGrader) Grade(_ context.Context, g sandbox.GradeSpec) (sandbox.GradeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Specs = append(f.Specs, g)
	var files []string
	filepath.WalkDir(g.App, func(p string, d fs.DirEntry, _ error) error {
		rel, _ := filepath.Rel(g.App, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	f.Apps = append(f.Apps, files)
	if f.Before != nil {
		f.Before(g)
	}
	if f.Err != nil {
		return sandbox.GradeResult{Image: "localhost/ygg-grader-fake"}, f.Err
	}
	if f.Forged != "" {
		if err := os.WriteFile(filepath.Join(g.Logs, "results.json"), []byte(f.Forged), 0o644); err != nil {
			return sandbox.GradeResult{}, err
		}
	}
	var stdout string
	switch {
	case f.Stdout != nil:
		stdout = f.Stdout(g.ScoreKey)
	case f.Results != "":
		stdout = "starting the app\n" + keyedLine(g.ScoreKey, f.Results) + "stopped the app\n"
	}
	return sandbox.GradeResult{Image: "localhost/ygg-grader-fake", Log: "suite output\n", Stdout: []byte(stdout)}, nil
}

// keyedLine is what a suite prints for Odin: the marker, the key and the report on one line.
func keyedLine(key, report string) string { return "ygg-results/v1 " + key + " " + report + "\n" }

// graded is a run with a sandbox, a grader block and a task: the pieces a test needs to look at.
type graded struct {
	r      *run.Run
	box    *sandboxfake.Sandbox
	grader *fakeGrader
	task   *tasktest.Task
}

// gradedPack is the sandbox pack with an acceptance stack, so a test can bring up a snapshot sandbox.
var gradedPack = strings.Replace(sandboxPack, "  egress:", "  acceptance: { stack: docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 }\n  egress:", 1)

const gradedNodes = buildNodes +
	"  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.implement/v1 }\n" +
	"  - { id: check, kind: exec, run: [grep, -q, done, build.txt], repo: repos/app }\n" +
	"  - { id: score, kind: grade }\n"

const gradedScript = buildScript +
	"  design:\n    - actions: [ { write: { path: out/design.md, content: \"# Design\\n\" } } ]\n      result: { status: ok, summary: designed, output: '{\"summary\": \"done\"}' }\n"

// gradedLimits is the limits part of the grader block; a test changes it for one run.
var gradedLimits = "  memory: 4g\n  cpus: 4\n"

// startGraded starts a run of nodes whose brief is the task's instruction.md, with the fake sandbox and the
// fake grader. results is what the fake suite writes.
func startGraded(t *testing.T, nodes, results string, edit func(*tasktest.Task)) *graded {
	t.Helper()
	tk := tasktest.New(t)
	if edit != nil {
		edit(tk)
	}
	realm, cfg := t.TempDir(), t.TempDir()
	project := treetest.Project(t, "repos/app")
	tmpl := filepath.Join(realm, "t")
	os.MkdirAll(tmpl, 0o755)
	files := map[string]string{
		filepath.Join(tmpl, "LICENSE"):     "MIT\n",
		filepath.Join(realm, "pack.yaml"):  "name: test\nversion: 0.1.0\ngraph: graph.yaml\n" + gradedPack,
		filepath.Join(realm, "graph.yaml"): "name: test\nversion: 1.0.0\nschema: 2\nnodes:\n" + nodes,
		filepath.Join(realm, "p.md"):       "Do the work.\n",
		filepath.Join(cfg, "script.yaml"):  gradedScript,
		filepath.Join(cfg, "factory.yaml"): "mode: unattended\ngates: { design: auto }\nroles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n" +
			"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n" +
			"sandbox:\n  engine: unix:///run/ygg-test.sock\n" +
			"grader:\n  tasks_dir: " + tk.TasksDir + "\n  suite_dir: " + tk.SuiteDir + "\n  timeout: 30m\n" + gradedLimits,
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &graded{box: &sandboxfake.Sandbox{}, grader: &fakeGrader{Results: results}, task: tk}
	h := run.Hooks{
		NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return g.box, nil },
		NewGrader:  func(*run.SandboxSetup, string) (run.Grader, error) { return g.grader, nil },
	}
	r, err := run.Start(context.Background(), run.Options{Project: project, Config: filepath.Join(cfg, "factory.yaml"), Graph: realm, Brief: tk.Brief(), Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	g.r = r
	return g
}

func (g *graded) advance(t *testing.T) Outcome {
	t.Helper()
	out, err := Advance(context.Background(), g.r)
	if err != nil {
		t.Fatalf("Advance() = %v", err)
	}
	return out
}

func readScore(t *testing.T, r *run.Run) gungnir.Score {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "score.json"))
	if err != nil {
		t.Fatalf("score.json: %v", err)
	}
	var s gungnir.Score
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGraderCombinesPublicTaskAndHiddenSuite(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(map[string]string{"a thing is private": "failed"}), nil)
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	if len(g.grader.Specs) != 1 {
		t.Fatalf("%d grading runs, want 1", len(g.grader.Specs))
	}
	spec := g.grader.Specs[0]
	// The image comes from the public task, the tests from the hidden store, and nothing else is given.
	if spec.Task != "tiny-task" || spec.Run != g.r.State.ID || spec.Environment != filepath.Join(g.task.Dir, "environment") || spec.Tests != g.task.Tests {
		t.Errorf("spec = %+v", spec)
	}
	if spec.App != filepath.Join(g.r.Dir, "grade", "app") || !strings.HasPrefix(spec.Logs, filepath.Join(g.r.Dir, "grade")+string(filepath.Separator)) {
		t.Errorf("the app and logs folders = %s, %s; want them in the run folder", spec.App, spec.Logs)
	}
	// 5 minutes is the task's verifier timeout, under the factory's 30.
	if spec.Timeout != 5*time.Minute || spec.Memory != "4g" || spec.CPUs != 4 || spec.Env["WORKERS"] != "2" || spec.Env["SCORE_WEIGHTS"] == "" {
		t.Errorf("limits = %v %s %d %v", spec.Timeout, spec.Memory, spec.CPUs, spec.Env)
	}
	sum := sha256.Sum256([]byte(tasktest.Brief))
	s := readScore(t, g.r)
	if s.Task != "tiny-task" || s.TaskVersion != "2.1.0" || s.Suite != "v1" || s.BriefSHA256 != hex.EncodeToString(sum[:]) || s.Run != g.r.State.ID {
		t.Errorf("score = %+v", s)
	}
	if s.FailToPass != (gungnir.Tally{Passed: 2, Total: 3}) || s.PassToPass != (gungnir.Tally{Passed: 1, Total: 1}) || s.Reward < 0.6-1e-9 || s.Reward > 0.6+1e-9 {
		t.Errorf("score = %+v", s)
	}
	text := ledgerText(t, g.r)
	for _, want := range []string{`"event":"grade_started"`, `"ygg.task.id":"tiny-task"`, `"ygg.grade.suite":"v1"`, `"ygg.grade.brief_sha256":"` + hex.EncodeToString(sum[:]) + `"`,
		`"ygg.grade.image":"localhost/ygg-grader-tiny-task:`, `"event":"grade_finished"`, `"ygg.grade.score_path":"` + filepath.Join(g.r.Dir, "score.json") + `"`,
		`"ygg.grade.fail_to_pass":"2/3"`, `"ygg.grade.pass_to_pass":"1/1"`, `"ygg.grade.reward":0.6`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(g.r.Dir, "grade", "grade.log")); string(b) != "suite output\n" {
		t.Errorf("grade.log = %q", b)
	}
}

// refusals writes the five ways grading is refused before a container is built.
var refusals = map[string]struct {
	edit   func(k *tasktest.Task)
	reason gungnir.GradeReason
}{
	"retired": {func(k *tasktest.Task) {
		os.MkdirAll(filepath.Join(k.Dir, "retired"), 0o755)
		os.WriteFile(filepath.Join(k.Dir, "retired", "v1"), []byte("retired\n"), 0o644)
	}, gungnir.GradeRetired},
	"missing_suite": {func(k *tasktest.Task) { os.RemoveAll(filepath.Join(k.SuiteDir, k.ID)) }, gungnir.GradeMissingSuite},
	"brief_changed": {func(k *tasktest.Task) { // task.toml still names the hash of the old text
		os.WriteFile(filepath.Join(k.Dir, "instruction.md"), []byte(tasktest.Brief+"One more line.\n"), 0o644)
	}, gungnir.GradeBriefChanged},
	"bad_manifest": {func(k *tasktest.Task) {
		os.WriteFile(filepath.Join(k.Tests, "manifest.json"), []byte(`{"schema":`), 0o644)
	}, gungnir.GradeBadManifest},
	"bad_task": {func(k *tasktest.Task) {
		b, _ := os.ReadFile(filepath.Join(k.Dir, "task.toml"))
		os.WriteFile(filepath.Join(k.Dir, "task.toml"), []byte(strings.Replace(string(b), `"separate"`, `"shared"`, 1)), 0o644)
	}, gungnir.GradeBadTask},
}

func TestEveryRefusalStopsTheRunBeforeAnythingIsBuilt(t *testing.T) {
	for name, c := range refusals {
		t.Run(name, func(t *testing.T) {
			g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
			c.edit(g.task)
			out := g.advance(t)
			if out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), string(c.reason)) {
				t.Fatalf("Advance() = %+v, want a failed run whose error says %s", out, c.reason)
			}
			if len(g.grader.Specs) != 0 {
				t.Errorf("the grader was called %d time(s) after a refusal", len(g.grader.Specs))
			}
			if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
				t.Error("a refused run has a score.json")
			}
			if _, err := os.Stat(filepath.Join(g.r.Dir, "grade", "app")); err == nil {
				t.Error("a refused run exported /app")
			}
			text := ledgerText(t, g.r)
			if !strings.Contains(text, `"event":"grade_refused"`) || !strings.Contains(text, `"ygg.grade.reason":"`+string(c.reason)+`"`) || strings.Contains(text, `"event":"grade_started"`) {
				t.Errorf("the ledger:\n%s", text)
			}
		})
	}
}

// An engine that cannot apply limits gets none: the operator wrote `memory: none` and `cpus: 0`, and the driver
// receives an empty memory and zero CPUs, not the words (the driver would refuse "none" as a limit).
func TestNoLimitGoesToTheDriverAsNoLimit(t *testing.T) {
	old := gradedLimits
	gradedLimits = "  memory: none\n  cpus: 0\n"
	defer func() { gradedLimits = old }()
	g := startGraded(t, gradedNodes, tasktest.Results(map[string]string{}), nil)
	if out := g.advance(t); out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v", out)
	}
	if spec := g.grader.Specs[0]; spec.Memory != "" || spec.CPUs != 0 {
		t.Errorf("limits = %q, %d; want none", spec.Memory, spec.CPUs)
	}
}

// A limit written on one side only is passed as written: 4g with no CPU limit, and no memory limit with 4 CPUs.
func TestMixedLimitsReachTheDriverAsWritten(t *testing.T) {
	for name, c := range map[string]struct {
		limits string
		memory string
		cpus   int
	}{
		"memory only": {"  memory: 4g\n  cpus: 0\n", "4g", 0},
		"cpus only":   {"  memory: none\n  cpus: 4\n", "", 4},
	} {
		t.Run(name, func(t *testing.T) {
			old := gradedLimits
			gradedLimits = c.limits
			defer func() { gradedLimits = old }()
			g := startGraded(t, gradedNodes, tasktest.Results(map[string]string{}), nil)
			if out := g.advance(t); out.Status != run.StatusDone {
				t.Fatalf("Advance() = %+v", out)
			}
			if spec := g.grader.Specs[0]; spec.Memory != c.memory || spec.CPUs != c.cpus {
				t.Errorf("limits = %q, %d; want %q, %d", spec.Memory, spec.CPUs, c.memory, c.cpus)
			}
		})
	}
}

// A task id that task.Load rejects must not reach the ledger: its event schema would refuse the line and the
// refusal would become an error of Odin's own.
func TestATaskIdThatIsNotATaskIdIsARefusalWithNoTaskInTheEvent(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	for _, id := range []string{"Smoke_Notes", ".."} {
		_, err := Grade(context.Background(), g.r, id)
		var refusal *task.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != gungnir.GradeBadTask {
			t.Fatalf("Grade(%q) = %v, want a bad_task refusal", id, err)
		}
	}
	if text := ledgerText(t, g.r); !strings.Contains(text, `"ygg.grade.reason":"bad_task"`) || strings.Contains(text, "Smoke_Notes") {
		t.Errorf("the ledger:\n%s", text)
	}
}

func TestABriefWhoseTaskIdIsNotATaskIdFailsTheGradeNodeAsARefusal(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), func(k *tasktest.Task) {
		bad := filepath.Join(k.TasksDir, "Bad_Id")
		if err := os.Rename(k.Dir, bad); err != nil {
			t.Fatal(err)
		}
		k.Dir = bad
	})
	out := g.advance(t)
	if out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), string(gungnir.GradeBadTask)) {
		t.Fatalf("Advance() = %+v, want a failed run whose error says bad_task", out)
	}
	if text := ledgerText(t, g.r); !strings.Contains(text, `"event":"grade_refused"`) || !strings.Contains(text, `"event":"node_failed"`) {
		t.Errorf("the ledger:\n%s", text)
	}
}

// brokenReleaser is a Releaser that fails until Err is nil, and then makes a one-file release.
type brokenReleaser struct {
	Err   error
	Calls int
	Dir   string // where the releases are made
}

func (b *brokenReleaser) Key() string { return "broken" }

func (b *brokenReleaser) Build(_ context.Context, spec run.ReleaseSpec) (string, error) {
	leaves := spec.Leaves
	b.Calls++
	if b.Err != nil {
		return "", b.Err
	}
	dir, err := os.MkdirTemp(b.Dir, "release")
	if err != nil {
		return "", err
	}
	for _, l := range leaves {
		os.MkdirAll(filepath.Join(dir, l), 0o755)
		os.WriteFile(filepath.Join(dir, l, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	}
	return dir, nil
}

// leakyReleaser makes a release and leaves its sandbox recorded as up, as a releaser whose sandbox did not stop.
type leakyReleaser struct {
	brokenReleaser
	r *run.Run
}

func (l *leakyReleaser) Build(ctx context.Context, spec run.ReleaseSpec) (string, error) {
	l.r.State.Sandboxes = map[string]gungnir.Env{"snapshot/release/test": {Name: "snapshot/release/test", Container: "c"}}
	return l.brokenReleaser.Build(ctx, spec)
}

// The second line behind the releaser's own stop: the grader does not start while a sandbox is recorded as up.
func TestTheGraderNeverStartsBesideTheReleaseSandbox(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	g.r.Releaser = &leakyReleaser{brokenReleaser{Dir: t.TempDir()}, g.r}
	commitRelease(t, g.r)
	_, err := Grade(context.Background(), g.r, "")
	var failed *GradeFailed
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "still up") {
		t.Fatalf("Grade() = %v, want a *GradeFailed for the sandbox left up", err)
	}
	if len(g.grader.Specs) != 0 {
		t.Errorf("the grader started beside a sandbox")
	}
}

// A leaf ships a release.sh and the project has no acceptance realm: the grade goes on without a release,
// and says so in the ledger and to the operator.
func TestAGradeWithNoAcceptanceRealmSaysThereIsNoRelease(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	commitRelease(t, g.r)
	graded, err := Grade(context.Background(), g.r, "")
	if err != nil {
		t.Fatal(err)
	}
	const want = "no release: the project has no acceptance realm"
	if !strings.HasPrefix(graded.Note, want) || !strings.Contains(graded.Note, "repos/app") {
		t.Errorf("Graded.Note = %q", graded.Note)
	}
	if text := ledgerText(t, g.r); !strings.Contains(text, `"event":"grade_started"`) || !strings.Contains(text, want) {
		t.Errorf("the ledger lacks the note:\n%s", text)
	}
}

// commitRelease commits an executable release.sh to repos/app and records it in the project.
func commitRelease(t *testing.T, r *run.Run) {
	t.Helper()
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.RepoDir("repos/app"), "release.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "release.sh"}, {"commit", "-q", "-m", "release"}} {
		if _, err := l.Git(context.Background(), args...); err != nil {
			t.Fatal(err)
		}
	}
	p := git.Repo{GitDir: r.Setup.Project.GitDir, WorkTree: r.Setup.Project.Dir}
	for _, args := range [][]string{{"add", "repos/app"}, {"commit", "-q", "-m", "bump"}} {
		if _, err := p.Run(context.Background(), args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGradeFailsOnARelease(t *testing.T) {
	g := startGraded(t, gradedNodes, tasktest.Results(nil), nil)
	rel := &brokenReleaser{Err: errors.New("go build: exit 2"), Dir: t.TempDir()}
	g.r.Releaser = rel
	commitRelease(t, g.r)
	_, err := Grade(context.Background(), g.r, "")
	var failed *GradeFailed
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "repos/app") || !strings.Contains(err.Error(), "exit 2") {
		t.Fatalf("Grade() = %v, want a *GradeFailed that names repos/app", err)
	}
	if _, err := os.Stat(filepath.Join(g.r.Dir, "score.json")); err == nil {
		t.Error("a failed release has a score")
	}
	if len(g.grader.Specs) != 0 {
		t.Errorf("the grader ran %d times on a project without its release", len(g.grader.Specs))
	}
	rel.Err = nil
	if _, err := Grade(context.Background(), g.r, ""); err != nil {
		t.Fatalf("the second Grade() = %v", err)
	}
	if len(g.grader.Specs) != 1 || rel.Calls != 2 {
		t.Errorf("grader calls %d, releaser calls %d; want 1 and 2", len(g.grader.Specs), rel.Calls)
	}
	found := false
	for _, f := range g.grader.Apps[0] {
		found = found || f == ".ygg/release/repos/app/run.sh"
	}
	if !found {
		t.Errorf("the graded /app lacks the release: %v", g.grader.Apps[0])
	}
}
