package run

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// startSandboxed starts a run of the sandboxed toy realm with a fake sandbox, and returns both.
func startSandboxed(t *testing.T) (*Run, *sandboxfake.Sandbox, string, Hooks) {
	t.Helper()
	project := treetest.Project(t, "repos/app")
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	r, err := Start(ctx, Options{Project: project, Config: sandboxConfig(t, script, ""), Realm: toySandbox(t), Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	return r, fake, project, h
}

func events(t *testing.T, r *Run) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Event)
	}
	return names
}

func TestRunnerStartsOneSandboxPerLeafAndRecordsIt(t *testing.T) {
	r, fake, project, _ := startSandboxed(t)
	defer r.Close()
	first, err := r.Runner(ctx, "repos/app")
	if err != nil || first == nil {
		t.Fatalf("Runner() = %v, %v", first, err)
	}
	if _, err := r.Runner(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if len(fake.Ups) != 1 {
		t.Fatalf("the sandbox came up %d times, want once per leaf", len(fake.Ups))
	}
	up := fake.Ups[0]
	leaf := r.RepoDir("repos/app")
	if up.Run != r.State.ID || up.Name != "repos/app" || up.Parent != project || up.Leaf != leaf ||
		up.GitDir != filepath.Join(project, ".git", "modules", "repos", "app") || strings.Join(up.Egress, ",") != "proxy.golang.org,sum.golang.org" {
		t.Errorf("spec = %+v", up)
	}
	if err := up.Validate(); err != nil {
		t.Errorf("the spec Odin built is one the driver would refuse: %v", err)
	}
	if env := r.State.Sandboxes["repos/app"]; env.Container == "" {
		t.Errorf("run.json doesn't record the sandbox: %+v", r.State.Sandboxes)
	}
	if got := events(t, r); got[len(got)-1] != "sandbox_up" {
		t.Errorf("events = %v, want sandbox_up last", got)
	}
	if umb, err := r.Runner(ctx, "."); err != nil || umb != nil {
		t.Errorf("Runner(umbrella) = %v, %v; Odin's generators run on the host", umb, err)
	}
}

func TestDownStopsWhatWasStartedAndForgetsIt(t *testing.T) {
	r, fake, _, _ := startSandboxed(t)
	defer r.Close()
	if _, err := r.Runner(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if err := r.DownSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Downs) != 1 || len(r.State.Sandboxes) != 0 {
		t.Errorf("downs %d, recorded %v", len(fake.Downs), r.State.Sandboxes)
	}
	if err := r.DownSandboxes(ctx); err != nil || len(fake.Downs) != 1 {
		t.Errorf("a second Down: %v, %d downs", err, len(fake.Downs))
	}
	if got := events(t, r); got[len(got)-1] != "sandbox_down" {
		t.Errorf("events = %v", got)
	}
}

func TestResumeStopsASandboxAKilledYggLeft(t *testing.T) {
	r, fake, project, h := startSandboxed(t)
	if _, err := r.Runner(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.Close() // as if ygg died: the sandbox is still recorded in run.json
	r2, err := OpenWith(ctx, project, id, "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if len(fake.Downs) != 1 || len(r2.State.Sandboxes) != 0 {
		t.Errorf("the leftover sandbox was not stopped: downs %d, recorded %v", len(fake.Downs), r2.State.Sandboxes)
	}
}

func TestASandboxThatWontStartIsAnError(t *testing.T) {
	r, fake, _, _ := startSandboxed(t)
	defer r.Close()
	fake.UpErr = &sandbox.PolicyError{Problems: []string{"devcontainer.json has initializeCommand"}}
	_, err := r.Runner(ctx, "repos/app")
	var pe *sandbox.PolicyError
	if !errors.As(err, &pe) {
		t.Errorf("Runner() = %v, want the *sandbox.PolicyError through", err)
	}
	if len(r.State.Sandboxes) != 0 {
		t.Error("a sandbox that failed to start was recorded")
	}
}

func TestAScriptedOnlyRunHasNoRunner(t *testing.T) {
	r := startToyRun(t, scriptedRoles+fullCaps)
	defer r.Close()
	if run, err := r.Runner(ctx, "repos/app"); run != nil || err != nil {
		t.Errorf("Runner() = %v, %v; want nil: the host runs it", run, err)
	}
}
