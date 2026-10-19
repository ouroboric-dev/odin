package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/scripted"
)

// The content smoke run (v0-H15) reads these files on a real engine; this test keeps them loadable.
func TestTheContentSmokeFilesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "smoke-content")
	if _, err := scripted.Load(filepath.Join(dir, "script.yaml")); err != nil {
		t.Errorf("script.yaml does not load: %v", err)
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
