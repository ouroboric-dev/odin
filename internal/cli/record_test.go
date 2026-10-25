package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
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
