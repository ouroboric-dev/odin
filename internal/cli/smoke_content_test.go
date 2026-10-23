package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/scripted"
	"github.com/ouroboric-dev/gungnir/schema"
	"go.yaml.in/yaml/v3"
)

// The content smoke run (v0-H15) reads these files on a real engine; this test keeps them loadable.
func TestTheContentSmokeFilesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "smoke-content")
	if _, err := scripted.Load(filepath.Join(dir, "script.yaml")); err != nil {
		t.Errorf("script.yaml does not load: %v", err)
	}
	// The leaves file the spec step writes parses, and pins the runtime realm the real helheim-go needs.
	raw, err := os.ReadFile(filepath.Join(dir, "script.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sc scripted.Script
	if err := yaml.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	var leavesFile string
	for _, a := range sc.Steps["spec"][0].Actions {
		if a.Write != nil && a.Write.Path == "out/leaves.yaml" {
			leavesFile = a.Write.Content
		}
	}
	leaves, err := schema.ParseLeaves("out/leaves.yaml", []byte(leavesFile))
	if err != nil {
		t.Errorf("the spec step's leaves file does not parse: %v", err)
	} else if leaves.Runtime == nil || leaves.Runtime.Realm != "niflheim-local" || leaves.Runtime.RealmVersion != "0.1.0" {
		t.Errorf("the leaves file pins the runtime %+v, want niflheim-local 0.1.0", leaves.Runtime)
	}
	brief, err := os.ReadFile(filepath.Join(dir, "brief.md"))
	if err != nil || len(strings.TrimSpace(string(brief))) == 0 {
		t.Errorf("brief.md: %q, %v", brief, err)
	}
	example, err := os.ReadFile(filepath.Join(dir, "factory.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "implementer", "reviewer"} {
		if !strings.Contains(string(example), role+": { agent: scripted, model: none, script: ") {
			t.Errorf("factory.example.yaml gives the %s no script", role)
		}
	}
	if !strings.Contains(string(example), "graphs:\n  dir: /home/OPERATOR/yggdrasil/voluspa") {
		t.Errorf("factory.example.yaml has no graphs: dir for the graph pack of Voluspa")
	}
}
