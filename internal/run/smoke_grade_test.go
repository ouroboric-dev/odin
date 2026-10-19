package run

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// realExample fills the places of testdata/smoke-grade/factory.real.example.yaml, as ygg-smoke-real.sh does,
// and writes the result to a config file. edit changes the text first.
func realExample(t *testing.T, edit func(string) string) (config string, tasks, suites string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke-grade", "factory.real.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	tasks, suites = filepath.Join(folder, "mimir"), filepath.Join(folder, "suites")
	for _, d := range []string{tasks, suites} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	text := strings.NewReplacer("@ENGINE@", "unix:///run/ygg-test.sock", "@FOLDER@", folder, "@BIND@", "/mnt/wsl/ygg-bind", "@TASKS@", tasks, "@SUITES@", suites).Replace(string(b))
	if edit != nil {
		text = edit(text)
	}
	if m := regexp.MustCompile(`@[A-Z]+@`).FindString(text); m != "" {
		t.Fatalf("the example has a place the script does not fill: %s", m)
	}
	config = filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(config, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return config, tasks, suites
}

// The grader smoke realm (v0-H16, v0-H17) is a loop and a grade node. With the real example config it must
// prepare with the two real adapters, real caps in every dimension, and a grade node at the end.
func TestTheGraderSmokeRealmPreparesWithRealAgentsAndRealCaps(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("ygg refuses to prepare a sandbox as root: the sandbox's user is the operator's own")
	}
	smoke, err := filepath.Abs("../../testdata/smoke-grade")
	if err != nil {
		t.Fatal(err)
	}
	config, tasks, suites := realExample(t, nil)
	s, err := Prepare(ctx, Options{Project: treetest.Project(t), Config: config, Realm: smoke})
	if err != nil {
		t.Fatal(err)
	}
	loop, last := s.Graph.Nodes[0], s.Graph.Nodes[len(s.Graph.Nodes)-1]
	if loop.Kind != "loop" || last.Kind != "grade" || s.Agents["implementer"].Name() != "claude-code" || s.Agents["reviewer"].Name() != "opencode" {
		t.Errorf("first node %+v, last node %+v, agents %v", loop, last, s.Agents)
	}
	if strings.Join(s.Sandbox.Egress, ",") != "api.anthropic.com,openrouter.ai" {
		t.Errorf("egress %v: the realm adds nothing to what the two adapters need", s.Sandbox.Egress)
	}
	// Every session has a turn cap and a time cap in the graph; the run has token, turn and dollar caps.
	if loop.MaxTurns != 40 || time.Duration(loop.Timeout) != 20*time.Minute || len(loop.Reviewers) != 1 ||
		loop.Reviewers[0].MaxTurns != 10 || time.Duration(loop.Reviewers[0].Timeout) != 5*time.Minute {
		t.Errorf("the loop's session caps: %+v", loop)
	}
	b := s.Config.Budget
	if b.PerRun.Tokens <= 0 || b.PerRun.Turns <= 0 || b.PerRun.USD <= 0 || b.PerDay.USD < b.PerRun.USD || b.PerDay.Tokens < b.PerRun.Tokens {
		t.Errorf("budget %+v: the run needs caps in tokens, turns and dollars, and the day at least the run's", b)
	}
	g := s.Config.Grader
	if mem, cpus := g.Limits(); g.TasksDir != tasks || g.SuiteDir != suites || mem != "" || cpus != 0 {
		t.Errorf("grader %+v: want the two folders, and no limit (memory: none, cpus: 0)", g)
	}
}

// A paid reviewer needs a dollar cap: the existing rule (change require-budget-caps) refuses the example without one.
func TestTheRealExampleNeedsItsDollarCaps(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"the run's dollar cap": func(s string) string { return strings.Replace(s, ", usd: 3", "", 1) },
		"the day's dollar cap": func(s string) string { return strings.Replace(s, ", usd: 6", "", 1) },
		"the run's turn cap":   func(s string) string { return strings.Replace(s, "tokens: 400000, turns: 100", "tokens: 400000", 1) },
	} {
		config, _, _ := realExample(t, edit)
		b, _ := os.ReadFile(config)
		_, err := schema.ParseFactory(config, b)
		var ce *schema.CapsError
		if !errors.As(err, &ce) {
			t.Errorf("without %s: ParseFactory() = %v, want a *CapsError", name, err)
		}
	}
}
