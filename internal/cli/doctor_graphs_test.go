package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
		"graph.yaml": "name: software\nversion: 0.1.0\nnodes:\n  - { id: a, kind: exec, run: [true], repo: . }\n",
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
	os.WriteFile(filepath.Join(store, "bare", "pack.yaml"), []byte("name: bare\nversion: 0.1.0\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(store, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(store, "other", "pack.yaml"), []byte("name: software\nversion: 0.1.0\ngraph: graph.yaml\n"), 0o644)
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
}
