package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/engine"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/task/tasktest"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const gradePack = "leaves:\n  repos/app: { description: the app, template: t, first_commit: [LICENSE] }\n" +
	"sandbox:\n  stack: docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514\n  egress: [proxy.golang.org]\n"

// engineStub puts a podman and a devcontainer that log what they are asked in front of the real ones. The
// podman answers `run` as a hidden suite would, by printing the results on its stdout on a line with the key
// it is given, and lists graderPS (a JSON array) for the containers that carry the grader label. Their answers are written
// into the scripts, because the driver passes the stubs no variable of the test's. It returns the log.
func engineStub(t *testing.T, results, graderPS string) string {
	t.Helper()
	bin, work := t.TempDir(), t.TempDir()
	log, res, ps := filepath.Join(work, "calls.log"), filepath.Join(work, "report.json"), filepath.Join(work, "ps.json")
	os.WriteFile(res, []byte(results), 0o644)
	os.WriteFile(ps, []byte(graderPS), 0o644)
	podman := "#!/bin/sh\necho \"podman $*\" >> " + log + "\ncase \"$1\" in\n  info) echo \"true 5.0.0\" ;;\n  image) exit 0 ;;\n" +
		"  ps) case \"$*\" in *ygg.grader*) cat " + ps + " ;; *) echo '[]' ;; esac ;;\n" +
		"  run) key=; for a in \"$@\"; do case \"$a\" in YGG_SCORE_KEY=*) key=${a#YGG_SCORE_KEY=} ;; esac; done\n" +
		"    echo 'starting the app'; echo \"ygg-results/v1 $key $(cat " + res + ")\" ;;\nesac\nexit 0\n"
	for name, body := range map[string]string{"podman": podman, "devcontainer": "#!/bin/sh\necho 0.89.0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// gradedRun makes a project and a config with a sandbox section and, when withGrader is set, a grader
// section; runs a one-node graph to its end with a fake sandbox, brief = the task's instruction.md; and
// returns the project, the config, the run's id and the task. A later ygg command opens the run as the
// real command line does, with the real driver, which the stubs of engineStub stand in for.
func gradedRun(t *testing.T, withGrader bool) (dir, config, id string, tk *tasktest.Task) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tk = tasktest.New(t)
	realm, cfgDir := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(realm, "t"), 0o755)
	script := "steps:\n  a:\n    - actions: [ { write: { path: build.txt, content: \"done\\n\" } } ]\n      result: { status: ok, summary: built }\n"
	cfg := "mode: unattended\nroles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n" +
		"budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\nsandbox:\n  engine: unix:///run/ygg-test.sock\n"
	if withGrader {
		cfg += "grader:\n  tasks_dir: " + tk.TasksDir + "\n  suite_dir: " + tk.SuiteDir + "\n  timeout: 30m\n  memory: 4g\n  cpus: 4\n"
	}
	for p, body := range map[string]string{
		filepath.Join(realm, "t", "LICENSE"):  "MIT\n",
		filepath.Join(realm, "pack.yaml"):     "name: test\nversion: 0.1.0\ngraph: graph.yaml\n" + gradePack,
		filepath.Join(realm, "graph.yaml"):    "name: test\nversion: 1.0.0\nschema: 1\nnodes:\n  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n",
		filepath.Join(realm, "p.md"):          "Do the work.\n",
		filepath.Join(cfgDir, "script.yaml"):  script,
		filepath.Join(cfgDir, "factory.yaml"): cfg,
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, config = treetest.Project(t, "repos/app"), filepath.Join(cfgDir, "factory.yaml")
	h := run.Hooks{NewSandbox: func(*run.SandboxSetup, string) (gungnir.Sandbox, error) { return &sandboxfake.Sandbox{}, nil }}
	r, err := run.Start(context.Background(), run.Options{Project: dir, Config: config, Graph: realm, Brief: tk.Brief(), Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if out, err := engine.Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	return dir, config, r.State.ID, tk
}

func TestGradeGradesADoneRunAndPrintsTheScore(t *testing.T) {
	dir, _, id, _ := gradedRun(t, true)
	log := engineStub(t, tasktest.Results(map[string]string{"a thing is private": "failed"}), "[]")
	code, out, errOut := ygg(t, "grade", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("grade: exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"Graded run " + id, "tiny-task 2.1.0", "suite v1", "reward:        0.6000", "fail-to-pass:  2/3", "pass-to-pass:  1/1",
		filepath.Join(run.Dir(dir, id), "score.json")} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dir, id), "score.json")); err != nil {
		t.Error(err)
	}
	calls, _ := os.ReadFile(log)
	var runLine string
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(l, "podman run ") {
			runLine = l
		}
	}
	// The real driver ran: no network, the run's label, three mounts, the suite's command, then the removal.
	for _, want := range []string{"--network none", "--label ygg.grader=" + id, "dst=/app", "dst=/tests,ro=true", "dst=/logs/verifier", "bash /tests/test.sh"} {
		if !strings.Contains(runLine, want) {
			t.Errorf("the run line lacks %q:\n%s", want, runLine)
		}
	}
	if n := strings.Count(runLine, "--mount"); n != 3 {
		t.Errorf("%d mounts, want 3", n)
	}
	if !strings.Contains(string(calls), "podman rm -f --ignore --time 0 ygg-grader-"+id+"-tiny-task") {
		t.Errorf("the container was not removed:\n%s", calls)
	}
}

func TestGradeRefusesWithExitTwoAndBuildsNothing(t *testing.T) {
	dir, _, id, tk := gradedRun(t, true)
	log := engineStub(t, tasktest.Results(nil), "[]")
	os.MkdirAll(filepath.Join(tk.Dir, "retired"), 0o755)
	os.WriteFile(filepath.Join(tk.Dir, "retired", "v1"), []byte("retired\n"), 0o644)
	code, out, errOut := ygg(t, "grade", id, "--project", dir)
	if code != ExitRefused || !strings.Contains(errOut, "refused (retired)") || strings.Contains(out, "reward") {
		t.Errorf("grade: exit %d\n%s%s", code, out, errOut)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "podman run") || strings.Contains(string(calls), "podman build") {
		t.Errorf("the engine was asked to build or run after a refusal:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dir, id), "score.json")); err == nil {
		t.Error("a refused grading left a score")
	}
	led, _ := os.ReadFile(filepath.Join(run.Dir(dir, id), "ledger.jsonl"))
	if !strings.Contains(string(led), `"event":"grade_refused"`) || !strings.Contains(string(led), `"ygg.grade.reason":"retired"`) {
		t.Errorf("the ledger lacks the refusal:\n%s", led)
	}
	// A task named on the command line must be a task of the store; the run's brief must be its brief.
	if code, _, errOut := ygg(t, "grade", id, "--project", dir, "--task", "no-such-task"); code != ExitRefused || !strings.Contains(errOut, "bad_task") {
		t.Errorf("grade --task no-such-task: exit %d, %q", code, errOut)
	}
	// An id that is not a task id at all is a refusal too, not an error of Odin's own.
	if code, _, errOut := ygg(t, "grade", id, "--project", dir, "--task", "Smoke_Notes"); code != ExitRefused || !strings.Contains(errOut, "bad_task") {
		t.Errorf("grade --task Smoke_Notes: exit %d, %q", code, errOut)
	}
}

func TestGradeNeedsADoneRunAndTheGraderSection(t *testing.T) {
	dir, _, id, _ := gradedRun(t, false)
	engineStub(t, tasktest.Results(nil), "[]")
	if code, _, errOut := ygg(t, "grade", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "grader") {
		t.Errorf("grade without a grader section: exit %d, %q", code, errOut)
	}
	if code, _, errOut := ygg(t, "grade", "20261015-090000-a1b2c3", "--project", dir); code != ExitRefused || !strings.Contains(errOut, "no run") {
		t.Errorf("grade of no run: exit %d, %q", code, errOut)
	}
	if code, _, _ := ygg(t, "grade", "--project", dir); code != ExitRefused {
		t.Errorf("grade without a run id: exit %d", code)
	}
	// A run that did not finish is not graded: its sandboxes may be up and its work is not final.
	p, _ := filepath.Abs("../../testdata/toy")
	dir2, config2 := project(t, "script.yaml", "")
	ygg(t, "run", "--project", dir2, "--config", config2, "--graph", p)
	if code, _, errOut := ygg(t, "grade", runID(t, dir2), "--project", dir2); code != ExitRefused {
		t.Errorf("grade of a stopped run: exit %d, %q", code, errOut)
	}
	// With the grader section there, the status alone refuses a run that did not finish.
	dir3, _, id3, _ := gradedRun(t, true)
	engineStub(t, tasktest.Results(nil), "[]")
	state := filepath.Join(dir3, ".factory", "runs", id3, "run.json")
	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), `"status": "done"`) {
		t.Fatalf("run.json = %v, %.200s", err, b)
	}
	os.WriteFile(state, []byte(strings.Replace(string(b), `"status": "done"`, `"status": "stopped"`, 1)), 0o644)
	if code, _, errOut := ygg(t, "grade", id3, "--project", dir3); code != ExitRefused || !strings.Contains(errOut, "stopped") {
		t.Errorf("grade of a stopped run with a grader section: exit %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir3, ".factory", "runs", id3, "score.json")); err == nil {
		t.Error("a run that did not finish has a score")
	}
}

func TestDoctorLeavesTheGraderContainerOfARunAliveYggHoldsAlone(t *testing.T) {
	dir, config, id, _ := gradedRun(t, true)
	log := engineStub(t, tasktest.Results(nil), `[{"Id":"abc123def456abc","State":"running","Labels":{"ygg.grader":"`+id+`"}}]`)
	release, err := lock.Take(filepath.Join(run.Dir(dir, id), "lock")) // a live ygg holds the run while it grades
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	code, out, errOut := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone {
		t.Fatalf("doctor: exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(out, "removed it") {
		t.Errorf("doctor removed the container of a run a live ygg holds:\n%s", out)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "podman rm") {
		t.Errorf("the engine was asked to remove it:\n%s", calls)
	}
}

func TestDoctorFindsAndRemovesALeftoverGraderContainer(t *testing.T) {
	dir, config, id, tk := gradedRun(t, true)
	other := "20200101-000000-aaaaaa"
	log := engineStub(t, tasktest.Results(nil), `[{"Id":"abc123def456abc","State":"exited","Labels":{"ygg.grader":"`+id+`"}},`+
		`{"Id":"zzz999zzz999zzz","State":"running","Labels":{"ygg.grader":"`+other+`"}}]`)
	code, out, errOut := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone {
		t.Fatalf("doctor: exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"OK    grader: 1 task(s) in " + tk.TasksDir,
		"WARN  graders: run " + id + " left the grader container abc123def456 (exited); removed it",
		"WARN  graders: container zzz999zzz999 of run " + other + " belongs to another project; doctor in that project removes it",
		"OK    graders: 2 grader container(s) of ygg runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "podman rm -f --ignore --time 0 abc123def456abc") || strings.Contains(string(calls), "rm -f --ignore --time 0 zzz999") {
		t.Errorf("the engine was asked:\n%s", calls)
	}
}

func TestDoctorChecksTheGraderSectionsFolders(t *testing.T) {
	dir, config, _, tk := gradedRun(t, true)
	engineStub(t, tasktest.Results(nil), "[]")
	b, _ := os.ReadFile(config)
	os.WriteFile(config, []byte(strings.Replace(string(b), "tasks_dir: "+tk.TasksDir, "tasks_dir: "+tk.TasksDir+"-gone", 1)), 0o644)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR grader: ") || !strings.Contains(out, "tasks_dir") {
		t.Errorf("doctor with no tasks folder: exit %d\n%s", code, out)
	}
	dir, config, _, _ = gradedRun(t, false)
	engineStub(t, tasktest.Results(nil), "[]")
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitDone || strings.Contains(out, "grader") {
		t.Errorf("doctor without a grader section: exit %d\n%s", code, out)
	}
}

func TestDoctorWarnsWhenTheGraderRunsWithNoLimit(t *testing.T) {
	dir, config, _, _ := gradedRun(t, true)
	engineStub(t, tasktest.Results(nil), "[]")
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); strings.Contains(out, "no memory or CPU limit") {
		t.Errorf("doctor warns about limits that are set:\n%s", out)
	}
	b, _ := os.ReadFile(config)
	os.WriteFile(config, []byte(strings.NewReplacer("memory: 4g", "memory: none", "cpus: 4", "cpus: 0").Replace(string(b))), 0o644)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || !strings.Contains(out, "WARN  grader: the grader's container runs with no memory or CPU limit") {
		t.Errorf("doctor with no limit: exit %d\n%s", code, out)
	}
}
