package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

func smokeRealms(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/smoke-realms")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// exactContent returns what a smoke prompt asks the agent to write: the text between "with exactly this
// content and nothing else in it:" and the line "Do nothing else".
func exactContent(t *testing.T, prompt string) string {
	t.Helper()
	b, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "with exactly this content and nothing else in it:\n\n")
	body, _, ok2 := strings.Cut(rest, "\nDo nothing else")
	if !ok || !ok2 {
		t.Fatalf("%s does not ask for exact content", prompt)
	}
	return strings.TrimRight(body, "\n") + "\n"
}

// indentBlock indents every line of s by n spaces, for a YAML block scalar.
func indentBlock(s string, n int) string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, strings.Repeat(" ", n)+l)
	}
	return strings.Join(out, "\n") + "\n"
}

// The realms smoke run (v0-H13) with real agents has a fixed shape; this runs the same graph with
// scripted agents that answer exactly what the prompts ask for. If a prompt asked for a document that Odin
// would refuse, the founder's run with real tokens would fail on it, and this test fails first.
func TestTheSmokeRealmsRunWithScriptedAgentsThatWriteWhatThePromptsAsk(t *testing.T) {
	lead := filepath.Join(smokeRealms(t), "smoke-lead")
	spec := exactContent(t, filepath.Join(lead, "prompts", "spec.md"))
	plan := exactContent(t, filepath.Join(lead, "prompts", "plan.md"))
	hello := "    - actions: [ { write: { path: hello.txt, content: \"hello from the factory\\n\" } } ]\n      result: { status: ok, summary: wrote hello.txt, output: '{\"summary\": \"done\"}' }\n"
	script := "steps:\n  spec:\n    - result:\n        status: ok\n        summary: answered with the spec\n        output: |\n" + indentBlock(spec, 10) +
		"  plan:\n    - result:\n        status: ok\n        summary: answered with the plan\n        output: |\n" + indentBlock(plan, 10) +
		"  smoke-t001-implement:\n" + hello + "  smoke-t002-implement:\n" + hello
	scriptPath := filepath.Join(t.TempDir(), "smoke.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := filepath.Join(t.TempDir(), "factory.yaml")
	body := "mode: unattended\ngates:\n  design: auto\nroles:\n  planner: { agent: scripted, model: none, script: " + scriptPath + " }\n  implementer: { agent: scripted, model: none, script: " + scriptPath + " }\n" +
		"budget:\n  per_run: { tokens: 600000, turns: 80 }\n  per_day: { tokens: 1500000, turns: 200 }\nrealms:\n  dir: " + smokeRealms(t) + "\ngraphs:\n  dir: " + smokeRealms(t) + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := treetest.Project(t)
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "smoke-lead")
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	u, err := schema.LoadUmbrella(dir)
	if err != nil || u.Graph != "smoke-lead" || u.Children["repos/api"].Realm != "smoke-api" || u.Children["repos/web"].Realm != "smoke-lead" || u.Children["repos/web"].Consumes[0] != "repos/api" {
		t.Errorf("umbrella.yaml = %+v, %v", u, err)
	}
	feats, err := schema.LoadFeatures(filepath.Join(dir, ".factory", "features.json"))
	if err != nil || len(feats.Tasks) != 2 || feats.Tasks[0].Status != schema.TaskDone || feats.Tasks[1].Status != schema.TaskDone {
		t.Errorf("features.json = %+v, %v", feats, err)
	}
	st, _ := run.LoadState(run.Dir(dir, runID(t, dir)))
	if len(st.Realms) != 2 || st.Leaves["repos/api"].Realm != "smoke-api" {
		t.Errorf("run.json realms %v, leaves %v", st.Realms, st.Leaves)
	}
}

// The example config, with the placeholders filled, prepares with the real adapter and finds both realms.
func TestTheSmokeRealmsPrepareWithTheRealAdapter(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(smokeRealms(t), "factory.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := filepath.Join(t.TempDir(), "factory.yaml")
	body := strings.ReplaceAll(string(b), "/home/OPERATOR/yggdrasil/odin/testdata/smoke-realms", smokeRealms(t))
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := run.Prepare(ctx, run.Options{Project: treetest.Project(t), Config: cfg, Graph: "smoke-lead"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents["planner"].Name() != "claude-code" || s.Agents["implementer"].Name() != "claude-code" || strings.Join(s.Sandbox.Hosts, ",") != "api.anthropic.com" {
		t.Errorf("agents %v, hosts %v", s.Agents, s.Sandbox.Hosts)
	}
	if got := strings.Join(s.Realms.Names(), ","); got != "smoke-lead" {
		t.Errorf("realms = %s; smoke-api joins when init creates its leaf", got)
	}
}

// The loops of the realm smoke graphs see what a loop is for (rulings R37, R52): the implementer its task, the open
// findings and how the checks ended, so a second attempt can fix a failing check; each reviewer the task, the change,
// the findings, the checks and a person's answer to its block.
func TestTheRealmSmokeLoopsDeclareWhatALoopIsFor(t *testing.T) {
	implementer := []string{schema.SourceTask, schema.SourceOpenFindings, schema.SourceCheckResults, schema.SourceRejection}
	reviewer := []string{schema.SourceTask, schema.SourceChange, schema.SourceOpenFindings, schema.SourceCheckResults, schema.SourceRejection}
	for _, p := range []string{"../../testdata/smoke-realms/smoke-lead/graph.yaml", "../../testdata/realms/midgard-toy/graph.yaml"} {
		g, err := schema.LoadGraph(p)
		if err != nil {
			t.Fatal(err)
		}
		loops := 0
		var walk func([]schema.Node)
		walk = func(nodes []schema.Node) {
			for _, n := range nodes {
				walk(n.Body)
				if n.Kind != schema.KindLoop {
					continue
				}
				loops++
				for _, in := range implementer {
					if !slices.Contains(n.Input, in) {
						t.Errorf("%s: loop %s does not declare %s: %v", p, n.ID, in, n.Input)
					}
				}
				for _, rv := range n.Reviewers {
					for _, in := range reviewer {
						if !slices.Contains(rv.Input, in) {
							t.Errorf("%s: reviewer %s of loop %s does not declare %s: %v", p, rv.ID, n.ID, in, rv.Input)
						}
					}
				}
			}
		}
		walk(g.Nodes)
		if loops == 0 {
			t.Errorf("%s has no loop", p)
		}
	}
}
