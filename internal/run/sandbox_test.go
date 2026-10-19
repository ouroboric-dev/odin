package run

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
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
	r, err := Start(ctx, Options{Project: project, Config: sandboxConfig(t, script, ""), Graph: toySandbox(t), Hooks: h})
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

// A resume after an interrupted node in a leaf with a sandbox discards the node's work without
// putting it in git: it was never scanned for secrets (decision 12). Commits a person made on the
// run's branch are already in git, and stay reachable from a ref.
func TestResumeNeverSavesUnscannedWorkToARef(t *testing.T) {
	r, _, project, h := startSandboxed(t)
	id, app := r.State.ID, r.RepoDir("repos/app")
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "by-hand.txt"), []byte("a person's commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, _, err := l.Checkpoint(ctx, "a person's commit after the checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	unscanned := "an interrupted node's unscanned work\n"
	if err := os.WriteFile(filepath.Join(app, "half-done.txt"), []byte(unscanned), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWith(ctx, project, id, "", h)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer r.Close()
	if _, err := os.Stat(filepath.Join(app, "half-done.txt")); err == nil {
		t.Error("Open() kept an interrupted node's work; the node restarts from its checkpoint")
	}
	blob := fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(unscanned), unscanned))))
	if _, err := l.Git(ctx, "cat-file", "-e", blob); err == nil {
		t.Error("the unscanned work is in the leaf's git objects")
	}
	refs, _ := l.Git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "refs/ygg/"+id+"/discarded/")
	if refs != "refs/ygg/"+id+"/discarded/1 "+commit {
		t.Errorf("the discarded refs are %q, want one at the person's commit %s and nothing else", refs, commit)
	}
	if len(r.Saved) != 1 || !strings.Contains(r.Saved[0], "not saved") || !strings.Contains(r.Saved[0], "refs/ygg/"+id+"/discarded/1") {
		t.Errorf("Saved = %q, want a note that the work was discarded unsaved, naming the ref of the commit", r.Saved)
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
