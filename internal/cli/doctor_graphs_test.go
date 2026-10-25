package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ouroboric-dev/odin/internal/run"
)

// graphStore writes a graph store holding the pack software (with a graph) and returns it.
func graphStore(t *testing.T) string {
	t.Helper()
	store := t.TempDir()
	dir := filepath.Join(store, "software")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"pack.yaml":  "name: software\nversion: 0.1.0\ngraph: graph.yaml\n",
		"graph.yaml": "name: software\nversion: 0.1.0\nschema: 2\nnodes:\n  - { id: a, kind: exec, run: [\"true\"], repo: . }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// withGraphs returns a copy of config that also has a graphs section for dir.
func withGraphs(t *testing.T, config, dir string) string {
	t.Helper()
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(out, append(b, []byte("graphs:\n  dir: "+dir+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDoctorListsTheGraphsOfTheStore(t *testing.T) {
	dir, plain := project(t, "script.yaml", "")
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", plain); !strings.Contains(out, "WARN  graphs: no graphs section: --graph takes a path, and a project can't pin a graph") {
		t.Errorf("doctor with no graphs section:\n%s", out)
	}
	store := graphStore(t)
	config := withGraphs(t, plain, store)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if want := "OK    graphs: " + store + " (software 0.1.0)"; !strings.Contains(out, want) || code != ExitDone {
		t.Errorf("doctor: exit %d, want %q in:\n%s", code, want, out)
	}
	// A pack in the store that holds no graph, and a folder that holds another graph than its name says.
	if err := os.MkdirAll(filepath.Join(store, "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "bare", "pack.yaml"), []byte("name: bare\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "other", "pack.yaml"), []byte("name: software\nversion: 0.1.0\ngraph: graph.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = ygg(t, "doctor", "--project", dir, "--config", config)
	for _, w := range []string{"ERROR graphs: bare: ", "holds no graph", "ERROR graphs: " + filepath.Join(store, "other") + " holds the graph software, so --graph other can't find it"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor's output lacks %q:\n%s", w, out)
		}
	}
	if code != ExitFailed {
		t.Errorf("exit %d; a pack with no graph in the store is an error", code)
	}
}

// A run folder from before graphs is not an ERROR for as long as it stays: doctor says how to be rid of it.
func TestDoctorWarnsOfARunFromBeforeGraphsAndStopsItsLeftoverProcess(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	id := "20261001-120000-abcdef"
	runDir := filepath.Join(dir, ".factory", "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sleep := exec.Command("sleep", "60")
	sleep.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sleep.Process.Kill(); sleep.Wait() })
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", sleep.Process.Pid))
	if err != nil {
		t.Skip("no /proc: the leftover check needs it")
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	state := fmt.Sprintf(`{"schema":"ygg.run/v2","id":%q,"status":"running","realm":"toy","exec":{"node":"build","pgid":%d,"start":%s},"some_field_v3_lacks":1}`,
		id, sleep.Process.Pid, fields[19])
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || strings.Contains(out, "ERROR") {
		t.Errorf("doctor: exit %d; a run from before graphs is a warning only:\n%s", code, out)
	}
	for _, w := range []string{"WARN  run " + id + ": ", "from before graphs", "can't be resumed", "remove its folder " + runDir,
		"WARN  run " + id + ": node build left process group " + fmt.Sprint(sleep.Process.Pid) + " running; stopped it"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor's output lacks %q:\n%s", w, out)
		}
	}
	// The process is really gone, not only reported so.
	gone := make(chan error, 1)
	go func() { gone <- sleep.Wait() }()
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Error("the leftover process is still running after doctor")
	}
}

// run.json is data: an exec record with a process group of 1 or less is a corrupt one, never a kill of
// every process the user owns. The start time is 0 so that a doctor that did not refuse it would not kill either.
func TestDoctorRefusesAnExecRecordWithAProcessGroupOfOneOrLess(t *testing.T) {
	for i, pgid := range []int{0, 1} {
		dir, config := project(t, "script.yaml", "")
		id := fmt.Sprintf("20261001-12000%d-abcdef", i)
		runDir := filepath.Join(dir, ".factory", "runs", id)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		state := fmt.Sprintf(`{"schema":"ygg.run/v2","id":%q,"status":"running","exec":{"node":"build","pgid":%d,"start":0}}`, id, pgid)
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
		if code != ExitFailed || !strings.Contains(out, "ERROR run "+id+": ") || !strings.Contains(out, "corrupt exec record") || strings.Contains(out, "stopped it") {
			t.Errorf("pgid %d: doctor exit %d, want an ERROR for a corrupt exec record:\n%s", pgid, code, out)
		}
	}
}

// withStores returns a copy of config with a realms section for realms and a graphs section for graphs.
func withStores(t *testing.T, config, realms, graphs string) string {
	t.Helper()
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(out, append(b, []byte("realms:\n  dir: "+realms+"\ngraphs:\n  dir: "+graphs+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// Ruling R16: when graphs.dir is realms.dir, a pack with no graph is a realm, not a broken graph pack.
func TestDoctorTakesAPackWithNoGraphForARealmWhenTheTwoStoresAreOneFolder(t *testing.T) {
	dir, plain := project(t, "script.yaml", "")
	store := graphStore(t) // holds the graph pack software
	realmDir := filepath.Join(store, "api-leaf")
	if err := os.MkdirAll(realmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realmDir, "pack.yaml"), []byte("name: api-leaf\nversion: 0.2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := func(out, prefix string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, prefix) {
				return l
			}
		}
		return ""
	}
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", withStores(t, plain, store, store))
	if got := line(out, "graphs: "); got != "OK    graphs: "+store+" (software 0.1.0)" {
		t.Errorf("shared store: graphs line = %q, want the graph pack listed and the realm pack not an error:\n%s", got, out)
	}
	if strings.Contains(out, "ERROR graphs") {
		t.Errorf("an ERROR for graphs in a store that is also the realm store:\n%s", out)
	}
	// Separate stores keep T011's rule.
	realms := t.TempDir()
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", withStores(t, plain, realms, store))
	if code != ExitFailed || !strings.Contains(out, "ERROR graphs: api-leaf: the pack holds no graph") {
		t.Errorf("separate stores: exit %d, want an ERROR for the pack with no graph:\n%s", code, out)
	}
	// A shared store that holds no graph pack at all, and a pack that doesn't load or is not its folder's, are errors.
	onlyRealms := t.TempDir()
	if err := os.MkdirAll(filepath.Join(onlyRealms, "api-leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(onlyRealms, "api-leaf", "pack.yaml"), []byte("name: api-leaf\nversion: 0.2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = ygg(t, "doctor", "--project", dir, "--config", withStores(t, plain, onlyRealms, onlyRealms))
	if !strings.Contains(out, "ERROR graphs: "+onlyRealms+" holds no graph pack") {
		t.Errorf("shared store with no graph pack:\n%s", out)
	}
	if err := os.MkdirAll(filepath.Join(store, "wrongname"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "wrongname", "pack.yaml"), []byte("name: other\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "broken", "pack.yaml"), []byte("name: [oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = ygg(t, "doctor", "--project", dir, "--config", withStores(t, plain, store, store))
	for _, w := range []string{"ERROR graphs: broken: ", "ERROR graphs: " + filepath.Join(store, "wrongname") + " holds the graph other"} {
		if !strings.Contains(out, w) {
			t.Errorf("shared store: output lacks %q:\n%s", w, out)
		}
	}
}

// The layout the smoke-realms example ships: one folder holds the graph pack and the leaf realms.
func TestDoctorGivesNoErrorForTheSmokeRealmsExampleLayout(t *testing.T) {
	dir, plain := project(t, "script.yaml", "")
	store, err := filepath.Abs("../../testdata/smoke-realms")
	if err != nil {
		t.Fatal(err)
	}
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", withStores(t, plain, store, store))
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "ERROR graphs") || strings.Contains(l, "ERROR realms") {
			t.Errorf("doctor on the smoke-realms layout: %s", l)
		}
	}
	if !strings.Contains(out, "OK    graphs: "+store+" (smoke-lead ") {
		t.Errorf("doctor does not list smoke-lead as the graph pack:\n%s", out)
	}
}

// A run from before graphs that a process still holds is being run by an older ygg: doctor says so, and does not
// tell the operator to remove the folder of a run that is still going.
func TestDoctorSaysAnOlderYggIsStillRunningARunBeforeGraphsAndKeepsItsFolder(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	id := "20261001-130000-abcdef"
	runDir := filepath.Join(dir, ".factory", "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(fmt.Sprintf(`{"schema":"ygg.run/v2","id":%q,"status":"running"}`, id)), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := run.Lock(runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || strings.Contains(out, "ERROR") {
		t.Errorf("doctor: exit %d; a run from before graphs is a warning only:\n%s", code, out)
	}
	if want := "WARN  run " + id + ": an older ygg is still running this run (schema ygg.run/v2); this ygg can't resume it"; !strings.Contains(out, want) {
		t.Errorf("doctor's output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "remove its folder") {
		t.Errorf("doctor tells the operator to remove the folder of a run that is still going:\n%s", out)
	}
}

// A prompt that backticks an input its session does not declare, in a graph pack or in the implementer prompt
// of a realm, is an error of doctor's, naming the file and the input (ruling R66).
func TestDoctorReportsAPromptThatNamesAnInputItsSessionDoesNotDeclare(t *testing.T) {
	dir, plain := project(t, "script.yaml", "")
	store := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(store, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("flow/pack.yaml", "name: flow\nversion: 0.1.0\ngraph: graph.yaml\n")
	write("flow/graph.yaml", "name: flow\nversion: 0.1.0\nschema: 2\nnodes:\n"+
		"  - { id: design, kind: agent, role: planner, prompt: prompts/design.md, scratch: true, input: [rejection], output: ygg.design/v1 }\n"+
		"  - { id: plan, kind: agent, role: planner, prompt: prompts/design.md, scratch: true, input: [rejection, refusal], output: ygg.plan/v1 }\n"+
		"  - { id: ready, kind: tasks, from: plan, leaves: docs/leaves.yaml, back_to: plan }\n"+
		"  - id: tasks\n    kind: foreach\n    over: tasks\n    nodes:\n"+
		"      - { id: implement, kind: loop, realm: leaf, role: implementer, attempts: 2, input: [task, rejection, refusal, open_findings, check_results], output: ygg.implement/v1 }\n"+
		"      - { id: land, kind: merge, realm: leaf, back_to: implement }\n")
	write("flow/prompts/design.md", "Write the design for the `brief`.\n")
	write("ground/pack.yaml", "name: ground\nversion: 0.1.0\nchecks:\n  unit: [test, -f, x]\nleaf:\n  description: A leaf.\n  template: templates/app\n  first_commit: [LICENSE]\n  implementer: prompts/implementer.md\n  checks: [unit]\n")
	write("ground/templates/app/LICENSE", "x\n")
	write("ground/prompts/implementer.md", "Build the `task`, then read the `design`.\n")
	config := withGraphs(t, plain, store)
	b, _ := os.ReadFile(config)
	config2 := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(config2, append(b, []byte("realms:\n  dir: "+store+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config2)
	for _, w := range []string{"ERROR graphs: flow: graph.yaml: node design: prompts/design.md", "prompts/design.md", "brief", "implementer prompt prompts/implementer.md", "the input design"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor's output lacks %q:\n%s", w, out)
		}
	}
	if code != ExitFailed {
		t.Errorf("exit %d, want %d", code, ExitFailed)
	}
}
