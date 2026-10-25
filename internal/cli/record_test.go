package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

func TestRecordJSONValidates(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	code, out, errOut := ygg(t, "record", id, "--project", dir)
	path := filepath.Join(run.Dir(dir, id), "record.json")
	if code != ExitDone || out != path+"\n" {
		t.Fatalf("record: exit %d, %q, %q; want 0 and the path", code, out, errOut)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if st.Mode().Perm() != 0o644 {
		t.Errorf("record.json has mode %v; want 0644, as the run's other files", st.Mode().Perm())
	}
	if err := schema.Validate(schema.ContractRecord, data); err != nil {
		t.Fatalf("record.json does not validate: %v\n%s", err, data)
	}
	var got struct {
		Run   string `json:"run"`
		Setup struct {
			Graph   map[string]string `json:"graph"`
			Prompts map[string]string `json:"prompts"`
			Roles   map[string]any    `json:"roles"`
		} `json:"setup"`
		Harness *struct{} `json:"harness"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Run != id || got.Setup.Graph["toy"] == "" || got.Setup.Prompts["prompts/design.md"] == "" ||
		got.Setup.Roles["planner"] == nil || got.Harness == nil {
		t.Errorf("record.json = %+v, %v:\n%s", got, err, data)
	}
	// ygg metrics --json prints the same document.
	code, out, _ = ygg(t, "metrics", id, "--project", dir, "--json")
	if code != ExitDone || out != string(data) {
		t.Errorf("metrics --json: exit %d, and its document is not record.json:\n%s", code, out)
	}
	// Rebuilt, it is the same.
	if code, _, _ := ygg(t, "record", id, "--project", dir); code != ExitDone {
		t.Fatalf("record again: exit %d", code)
	}
	again, err := os.ReadFile(path)
	if err != nil || string(again) != string(data) {
		t.Errorf("a rebuilt record differs: %v", err)
	}
	entries, err := os.ReadDir(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left: %s", e.Name())
		}
	}
}

func TestRecordTakesOneRun(t *testing.T) {
	dir, _ := project(t, "script.yaml", "")
	for name, args := range map[string][]string{
		"a range":     {"record", "20261015-090000-a1b2c3..20261015-090000-a1b2c4", "--project", dir},
		"no such run": {"record", "20261015-090000-a1b2c3", "--project", dir},
		"a path":      {"record", "../..", "--project", dir},
		"no run":      {"record", "--project", dir},
	} {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, code, ExitRefused)
		}
	}
}

// runGraph runs graph in dir to its end, approving every stop, and returns the new run's id.
func runGraph(t *testing.T, dir, config, graph string) string {
	t.Helper()
	known := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	for _, e := range entries {
		known[e.Name()] = true
	}
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", graph)
	entries, _ = os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	var id string
	for _, e := range entries {
		if !known[e.Name()] {
			id = e.Name()
		}
	}
	for code == ExitStopped {
		s, err := run.LoadState(run.Dir(dir, id))
		if err != nil {
			t.Fatal(err)
		}
		code, out, errOut = ygg(t, "approve", id, s.Pending, "--project", dir)
	}
	if code != ExitDone || id == "" {
		t.Fatalf("exit %d, run %q:\n%s%s", code, id, out, errOut)
	}
	return id
}

// recordOf runs ygg record for id and returns the record's setup.
func recordOf(t *testing.T, dir, id string) metrics.Setup {
	t.Helper()
	if code, out, errOut := ygg(t, "record", id, "--project", dir); code != ExitDone {
		t.Fatalf("record %s: exit %d:\n%s%s", id, code, out, errOut)
	}
	data, err := os.ReadFile(filepath.Join(run.Dir(dir, id), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Setup metrics.Setup `json:"setup"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r.Setup
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The record's prompt hashes are what the run loaded, kept in run.json when it started: a prompt edited after the
// run, and not committed, does not change them. A new run on the edited prompt records its new hash.
func TestTheRecordHoldsThePromptsTheRunUsed(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	graph := filepath.Join(t.TempDir(), "toy")
	if err := os.CopyFS(graph, os.DirFS(toyRealm(t))); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(graph, "prompts", "design.md")
	before, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	first := runGraph(t, dir, config, graph)
	s, err := run.LoadState(run.Dir(dir, first))
	if err != nil {
		t.Fatal(err)
	}
	if s.PromptSHA256["prompts/design.md"] != sha(before) {
		t.Errorf("run.json keeps the prompt hashes %v, want prompts/design.md at %s", s.PromptSHA256, sha(before))
	}
	if err := os.WriteFile(prompt, append(before, "Edited after the run.\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := recordOf(t, dir, first).Prompts["prompts/design.md"]; got != sha(before) {
		t.Errorf("the record of the first run hashes the design prompt as %s, want the hash the run used, %s", got, sha(before))
	}
	second := runGraph(t, dir, config, graph)
	a, b := recordOf(t, dir, first), recordOf(t, dir, second)
	if a.Prompts["prompts/design.md"] == b.Prompts["prompts/design.md"] {
		t.Error("the second run used the edited prompt, and its record has the first run's hash")
	}
	if a.Prompts["prompts/build.md"] == "" || a.Prompts["prompts/build.md"] != b.Prompts["prompts/build.md"] {
		t.Errorf("the build prompt did not change, and its hashes are %q and %q", a.Prompts["prompts/build.md"], b.Prompts["prompts/build.md"])
	}
}

// Two runs on the same files have the same setup, field by field.
func TestTwoRunsOnTheSameFilesRecordTheSameSetup(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	first, _ := walk(t, dir, config)
	second := anotherRun(t, dir, config)
	a, b := recordOf(t, dir, first), recordOf(t, dir, second)
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range av.NumField() {
		if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			t.Errorf("setup.%s differs: %v and %v", av.Type().Field(i).Name, av.Field(i).Interface(), bv.Field(i).Interface())
		}
	}
	if len(a.Prompts) == 0 || len(a.Graph) == 0 || len(a.Roles) == 0 {
		t.Errorf("the setup is nearly empty: %+v", a)
	}
}
