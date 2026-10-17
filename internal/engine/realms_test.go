package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// A realm for a test: its name and version, the lines of its pack.yaml after those, and its files.
type realmFiles struct {
	name, version, pack string
	files               map[string]string
}

// realmsSetup says how startRealms makes a run with several realms. The first realm leads, with the
// nodes; leaves maps each leaf of the project to the realm it is pinned to ("" pins none).
type realmsSetup struct {
	mode, nodes, script, roles, budget string
	realms                             []realmFiles
	leaves                             map[string]string
	hooks                              run.Hooks
}

const defaultRoles = "roles:\n  planner: { agent: scripted, model: none, script: script.yaml }\n  implementer: { agent: scripted, model: none, script: script.yaml }\n  reviewer: { agent: scripted, model: none, script: script.yaml }\n"

// startRealms writes a realm store, a project pinned to those realms and a factory.yaml, and starts a run.
func startRealms(t *testing.T, o realmsSetup) *run.Run {
	t.Helper()
	store, cfg := t.TempDir(), t.TempDir()
	lead := o.realms[0]
	for i, r := range o.realms {
		dir := filepath.Join(store, r.name)
		files := map[string]string{"pack.yaml": "name: " + r.name + "\nversion: " + r.version + "\n" + r.pack}
		if i == 0 {
			files["pack.yaml"] += "graph: graph.yaml\n"
			files["graph.yaml"] = "name: " + r.name + "\nversion: 1.0.0\nnodes:\n" + o.nodes
		}
		for f, body := range r.files {
			files[f] = body
		}
		for f, body := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	version := map[string]string{}
	for _, r := range o.realms {
		version[r.name] = r.version
	}
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Pin: treetest.Pin{Realm: lead.name, Version: lead.version}, Children: map[string]treetest.Node{}}
	for path, name := range o.leaves {
		var pin treetest.Pin
		if name != "" {
			pin = treetest.Pin{Realm: name, Version: version[name]}
		}
		root.Children[path] = treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: pin}
	}
	project := treetest.Build(t, root)
	if o.mode == "" {
		o.mode = "key-points"
	}
	if o.roles == "" {
		o.roles = defaultRoles
	}
	if o.budget == "" {
		o.budget = "budget:\n  per_run: { tokens: 1000000, turns: 1000 }\n  per_day: { tokens: 2000000, turns: 2000 }\n"
	}
	o.budget = strings.TrimRight(o.budget, "\n") + "\n  journal: " + filepath.Join(cfg, "spend") + "\n"
	for f, body := range map[string]string{
		"script.yaml":  o.script,
		"factory.yaml": "mode: " + o.mode + "\n" + o.roles + o.budget + "realms:\n  dir: " + store + "\n",
	} {
		if err := os.WriteFile(filepath.Join(cfg, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := run.Start(context.Background(), run.Options{Project: project, Config: filepath.Join(cfg, "factory.yaml"), Realm: lead.name, Hooks: o.hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// transcriptPrompt returns the prompt a scripted session of node got in attempt 1.
func transcriptPrompt(t *testing.T, r *run.Run, node string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", node+"-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr struct{ Prompt string }
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	return tr.Prompt
}

const leadAndOther = "checks:\n  unit: [sh, -c, \"echo lead > ran-by.txt\"]\n"

func twoRealms() []realmFiles {
	return []realmFiles{
		{name: "midgard-react", version: "0.1.0", pack: leadAndOther, files: map[string]string{"p.md": "Lead prompt.\n"}},
		{name: "helheim-go", version: "0.2.0", pack: "checks:\n  unit: [sh, -c, \"echo other > ran-by.txt\"]\n", files: map[string]string{"prompts/implementer.md": "Other realm's implementer prompt.\n"}},
	}
}

func TestANodeReadsItsPromptAndRunsItsChecksFromItsOwnRealm(t *testing.T) {
	r := startRealms(t, realmsSetup{realms: twoRealms(), leaves: map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"},
		nodes:  "  - { id: web-build, kind: agent, role: implementer, prompt: p.md, repo: repos/web }\n  - { id: api-touch, kind: exec, run: [echo, api], repo: repos/api }\n",
		script: "steps:\n  api-build:\n    - result: { status: ok, summary: built }\n  web-build:\n    - result: { status: ok, summary: built }\n"})
	ctx := context.Background()
	api := schema.Node{ID: "api-build", Kind: schema.KindAgent, Role: "implementer", Prompt: "prompts/implementer.md", Repo: "repos/api", Realm: "helheim-go", Checks: []string{"unit"}}
	if err := runAgent(ctx, r, api); err != nil {
		t.Fatalf("runAgent() = %v", err)
	}
	if got := transcriptPrompt(t, r, "api-build"); !strings.Contains(got, "Other realm's implementer prompt.") {
		t.Errorf("the api's implementer got %q; its prompt is in the realm of its leaf", got)
	}
	web := schema.Node{ID: "web-build", Kind: schema.KindAgent, Role: "implementer", Prompt: "p.md", Repo: "repos/web"}
	if err := runAgent(ctx, r, web); err != nil {
		t.Fatalf("runAgent() = %v", err)
	}
	if got := transcriptPrompt(t, r, "web-build"); !strings.Contains(got, "Lead prompt.") {
		t.Errorf("a node with no realm got %q; it is the lead realm's", got)
	}
	if err := runChecks(ctx, r, api, 1, api.Checks, "repos/api", r.RepoDir("repos/api")); err != nil {
		t.Fatalf("runChecks() = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.RepoDir("repos/api"), "ran-by.txt")); strings.TrimSpace(string(b)) != "other" {
		t.Errorf("the api's check was run by %q; the check of helheim-go is the one to run", b)
	}
	web.Checks = []string{"unit"}
	if err := runChecks(ctx, r, web, 1, web.Checks, "repos/web", r.RepoDir("repos/web")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.RepoDir("repos/web"), "ran-by.txt")); strings.TrimSpace(string(b)) != "lead" {
		t.Errorf("the web's check was run by %q", b)
	}
}
