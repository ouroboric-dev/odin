package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/models"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// fakeModels stands in for the model server: it records each Ready, and what had started by then.
type fakeModels struct {
	mu    sync.Mutex
	err   error
	calls []string
	// at each call: the sandboxes up and whether the runs folder held anything
	ups   []int
	runs  []bool
	fake  *sandboxfake.Sandbox
	runsd string
}

func (f *fakeModels) Ready(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.fake != nil {
		f.ups = append(f.ups, len(f.fake.Ups))
	}
	if f.runsd != "" {
		entries, _ := os.ReadDir(f.runsd)
		f.runs = append(f.runs, len(entries) > 0)
	}
	return f.err
}

// writeModelList writes a model list that holds the named models, and returns its path.
func writeModelList(t *testing.T, path string, names ...string) string {
	t.Helper()
	body := "server: { name: llama, network: ygg-models, port: 8081, host_port: 18081, weights: /srv/ygg-models/weights, state: " + t.TempDir() + " }\nmodels:\n"
	for _, n := range names {
		body += "  " + n + ": { file: " + n + ".gguf, context: 32768, output: 4096, price: { input: 0.1, output: 0.2 } }\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// localSetup is a project, a model list and the hooks of a local run: a fake sandbox and a fake model server.
type localSetup struct {
	project, list string
	fake          *sandboxfake.Sandbox
	server        *fakeModels
	hooks         Hooks
}

func newLocalSetup(t *testing.T, names ...string) *localSetup {
	t.Helper()
	l := &localSetup{project: treetest.Project(t, "repos/app"), fake: &sandboxfake.Sandbox{}}
	l.list = writeModelList(t, filepath.Join(t.TempDir(), "models.yaml"), names...)
	l.server = &fakeModels{fake: l.fake, runsd: filepath.Join(l.project, ".factory", "runs")}
	l.hooks = Hooks{
		NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return l.fake, nil },
		NewModels:  func(*schema.ModelList) models.Server { return l.server },
	}
	return l
}

// config writes a factory.yaml with a sandbox section, the model list, the given planner and implementer, and extra.
func (l *localSetup) config(t *testing.T, planner, implementer, extra string) string {
	t.Helper()
	body := "roles:\n  planner: " + planner + "\n  implementer: " + implementer + "\n" + fullCaps + sandboxSection +
		"models:\n  file: " + l.list + "\n" + extra
	p := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func scriptedRole(t *testing.T) string {
	return "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
}

func noRunFolder(t *testing.T, project string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(project, ".factory", "runs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("the refusal left a run folder: %v", entries)
	}
}

func TestLocalRunRefusesTwoModels(t *testing.T) {
	t.Run("a planner and an implementer", func(t *testing.T) {
		l := newLocalSetup(t, "alpha", "beta")
		cfg := l.config(t, "{ agent: opencode, model: local/alpha }", "{ agent: opencode, model: local/beta }", "")
		_, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
		if err == nil || !strings.Contains(err.Error(), "local/alpha") || !strings.Contains(err.Error(), "local/beta") {
			t.Fatalf("Start() = %v, want a refusal that names local/alpha and local/beta", err)
		}
		if len(l.server.calls) != 0 {
			t.Errorf("Ready was called %v; a refused run asks the server nothing", l.server.calls)
		}
		noRunFolder(t, l.project)
	})
	t.Run("a planner and a loop's reviewer", func(t *testing.T) {
		l := newLocalSetup(t, "alpha", "beta")
		graph := loopRealm(t, map[string]func(string) string{"graph.yaml": func(g string) string {
			return strings.Replace(g, "nodes:\n", "nodes:\n  - { id: design, kind: agent, role: planner, prompt: prompts/build.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n", 1)
		}})
		cfg := writeConfig(t, t.TempDir(), "roles:\n  planner: { agent: opencode, model: local/alpha }\n  implementer: { agent: opencode, model: local/alpha }\n"+
			"  reviewer: { agent: opencode, model: local/beta }\n"+fullCaps+sandboxSection+"models:\n  file: "+l.list+"\n")
		_, err := Prepare(ctx, Options{Project: treetest.Project(t, "repos/app"), Config: cfg, Graph: graph, Hooks: l.hooks})
		if err == nil || !strings.Contains(err.Error(), "local/alpha") || !strings.Contains(err.Error(), "local/beta") {
			t.Fatalf("Prepare() = %v, want a refusal that names local/alpha and local/beta", err)
		}
	})
	t.Run("a role the graph does not use counts for nothing", func(t *testing.T) {
		l := newLocalSetup(t, "alpha", "beta")
		cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
		b, _ := os.ReadFile(cfg)
		b = []byte(strings.Replace(string(b), "roles:\n", "roles:\n  spare: { agent: opencode, model: local/beta }\n", 1))
		os.WriteFile(cfg, b, 0o644)
		s, err := Prepare(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
		if err != nil {
			t.Fatalf("Prepare() = %v; the graph's roles name one local model", err)
		}
		if s.LocalModel != "alpha" {
			t.Errorf("LocalModel = %q, want alpha", s.LocalModel)
		}
	})
}

func TestLocalStartLeavesNothingWhenTheServerIsDown(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	l.server.err = errors.New(`model "alpha": the model server does not answer at http://127.0.0.1:18081 (start it: scripts/ygg-models.sh start)`)
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err == nil {
		r.Close()
		t.Fatal("Start() = nil, want the server's error")
	}
	if !errors.Is(err, l.server.err) {
		t.Errorf("Start() = %v, want the server's error through", err)
	}
	if strings.Join(l.server.calls, ",") != "alpha" {
		t.Errorf("Ready calls = %v, want one for alpha", l.server.calls)
	}
	noRunFolder(t, l.project)
	if len(l.fake.Ups) != 0 {
		t.Errorf("%d sandboxes came up", len(l.fake.Ups))
	}
}

func TestLocalStartReadiesTheModelBeforeTheRunIsRecorded(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if strings.Join(l.server.calls, ",") != "alpha" || len(l.server.runs) != 1 || l.server.runs[0] {
		t.Errorf("Ready calls = %v, runs folder held something at the call: %v; want one call for alpha before the run folder", l.server.calls, l.server.runs)
	}
}

func TestLocalResumeLoadsTheModel(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Runner(ctx, "repos/app"); err != nil { // a sandbox a killed ygg left
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close()
	l.server.calls, l.server.ups, l.server.runs = nil, nil, nil
	r2, err := OpenWith(ctx, l.project, id, "", l.hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if strings.Join(l.server.calls, ",") != "alpha" {
		t.Fatalf("Ready calls on resume = %v, want one for alpha", l.server.calls)
	}
	// Ready ran before the resume touched a sandbox: the leftover was still up, and nothing new had started.
	if len(l.server.ups) != 1 || l.server.ups[0] != 1 || len(l.fake.Downs) != 1 || len(l.fake.Ups) != 1 {
		t.Errorf("at Ready, sandboxes up %v; after: ups %d, downs %d", l.server.ups, len(l.fake.Ups), len(l.fake.Downs))
	}

	// A resume whose server is down is refused, and leaves the run as it was.
	l.server.calls, l.server.err = nil, errors.New("the model server does not answer")
	r2.Close()
	if _, err := OpenWith(ctx, l.project, id, "", l.hooks); !errors.Is(err, l.server.err) {
		t.Errorf("OpenWith() = %v, want the server's error", err)
	}
}

func TestLocalResumeRefusesAModelGoneFromTheList(t *testing.T) {
	l := newLocalSetup(t, "alpha", "beta")
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close()
	writeModelList(t, l.list, "beta")
	l.server.calls = nil
	_, err = OpenWith(ctx, l.project, id, "", l.hooks)
	// Prepare's own refusal, which names the list, not only the adapter's.
	if err == nil || !strings.Contains(err.Error(), "local/alpha") || !strings.Contains(err.Error(), l.list) {
		t.Fatalf("OpenWith() = %v, want a refusal that names local/alpha and the list %s", err, l.list)
	}
	if len(l.server.calls) != 0 {
		t.Errorf("Ready calls = %v; a refused resume asks the server nothing", l.server.calls)
	}
}

func TestLocalSandboxesHoldTheModelNetwork(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	store := t.TempDir()
	sandboxRealm(t, store, "toy-local", "0.1.0", nodeStack, "registry.npmjs.org", "")
	withAcceptance(t, store, "toy-local")
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "toy-local", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "toy-local", Version: "0.1.0"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "toy-local", Version: "0.1.0"}},
	}}
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n")
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: cfg, Graph: "toy-local", Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	want := gungnir.ModelServer{Network: "ygg-models", Name: "llama", Port: 8081}
	if m := r.Setup.Sandbox.Models; m == nil || *m != want {
		t.Errorf("SandboxSetup.Models = %+v, want %+v", m, want)
	}
	if _, err := r.Runner(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ScratchRunner(ctx, "design"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err != nil {
		t.Fatal(err)
	}
	if len(l.fake.Ups) != 3 {
		t.Fatalf("%d sandboxes came up, want 3", len(l.fake.Ups))
	}
	for _, up := range l.fake.Ups[:2] {
		if up.Models == nil || *up.Models != want {
			t.Errorf("%s: Models = %+v, want %+v", up.Name, up.Models, want)
		}
		if err := up.Validate(); err != nil {
			t.Errorf("%s: the spec Odin built is one the driver would refuse: %v", up.Name, err)
		}
	}
	if snap := l.fake.Ups[2]; snap.Models != nil {
		t.Errorf("%s: Models = %+v; a snapshot sandbox never joins the model network", snap.Name, snap.Models)
	}
}

func TestRunWithoutLocalRolesIsUnchanged(t *testing.T) {
	l := newLocalSetup(t, "alpha") // a model list, and no role on it
	cfg := l.config(t, scriptedRole(t), scriptedRole(t), "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	if r.Setup.LocalModel != "" || r.Setup.Sandbox.Models != nil {
		t.Errorf("LocalModel %q, SandboxSetup.Models %+v; want none", r.Setup.LocalModel, r.Setup.Sandbox.Models)
	}
	if _, err := r.Runner(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ScratchRunner(ctx, "design"); err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close()
	r2, err := OpenWith(ctx, l.project, id, "", l.hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	for _, up := range l.fake.Ups {
		if up.Models != nil {
			t.Errorf("%s: Models = %+v; a run with no local role joins no model network", up.Name, up.Models)
		}
	}
	if len(l.server.calls) != 0 {
		t.Errorf("Ready calls = %v; a run with no local role never asks the server", l.server.calls)
	}
}

// A done run starts no session: opening it, as the bridge's export does, asks the model server nothing.
func TestLocalDoneRunNeedsNoModel(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.State.Status = StatusDone
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	l.server.calls, l.server.err = nil, errors.New("the model server does not answer")
	r2, err := OpenWith(ctx, l.project, id, "", l.hooks)
	if err != nil {
		t.Fatalf("OpenWith() = %v; a done run needs no model", err)
	}
	r2.Close()
	if len(l.server.calls) != 0 {
		t.Errorf("Ready calls = %v; a done run asks the server nothing", l.server.calls)
	}
}

// Export runs no session: opening a failed local run the way it does asks the model server nothing, and a resume
// still asks it once.
func TestLocalExportOpenNeedsNoModelServer(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	cfg := l.config(t, scriptedRole(t), "{ agent: opencode, model: local/alpha }", "")
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.State.Status = StatusFailed
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	l.server.calls, l.server.err = nil, errors.New("the model server does not answer")
	r2, err := OpenForExport(ctx, l.project, id, l.hooks)
	if err != nil {
		t.Fatalf("OpenForExport() = %v; an export needs no model server", err)
	}
	r2.Close()
	if len(l.server.calls) != 0 {
		t.Errorf("Ready calls = %v; an export asks the server nothing", l.server.calls)
	}
	l.server.err = nil
	r3, err := OpenWith(ctx, l.project, id, "", l.hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	if strings.Join(l.server.calls, ",") != "alpha" {
		t.Errorf("Ready calls on resume = %v, want one for alpha", l.server.calls)
	}
}

// A model list is read for a local role only: a config that has a models section and no local role in the graph
// loads without the list.
func TestPrepareReadsNoModelListWithoutALocalRole(t *testing.T) {
	l := newLocalSetup(t, "alpha")
	cfg := l.config(t, scriptedRole(t), scriptedRole(t), "")
	if err := os.Remove(l.list); err != nil {
		t.Fatal(err)
	}
	r, err := Start(ctx, Options{Project: l.project, Config: cfg, Graph: toySandbox(t), Hooks: l.hooks})
	if err != nil {
		t.Fatalf("Start() = %v; no role is local, so the list is not read", err)
	}
	r.Close()
}
