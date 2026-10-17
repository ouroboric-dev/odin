package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

const scratchNode = "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, inputs: [docs/input.md, docs/more, missing.md] }\n"

const designScript = "steps:\n  design:\n    - actions:\n        - write: { path: out/docs/design.md, content: \"# Design\\n\" }\n        - write: { path: docs/input.md, content: \"changed in the copy\\n\" }\n" +
	"      result: { status: ok, summary: wrote the design }\n" +
	"    - actions: [ { write: { path: out/docs/other.md, content: \"# Other\\n\" } } ]\n      result: { status: ok, summary: wrote another design }\n"

// withInputs writes the project files a scratch agent is given copies of.
func withInputs(t *testing.T, r *run.Run) {
	t.Helper()
	for p, body := range map[string]string{"docs/input.md": "the input\n", "docs/more/a.md": "a\n", "docs/more/.git/HEAD": "ref\n"} {
		f := filepath.Join(r.Project, p)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAScratchAgentWorksInAWorkspaceOfItsOwnAndWritesUnderOut(t *testing.T) {
	r := start(t, "key-points", "", scratchNode, designScript)
	withInputs(t, r)
	r.Setup.BriefText = "Build a gallery."
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	ws := r.ScratchDir("design")
	if ws != filepath.Join(r.Dir, "scratch", "design") {
		t.Errorf("ScratchDir = %s", ws)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "out", "docs", "design.md")); err != nil || string(b) != "# Design\n" {
		t.Errorf("out/docs/design.md = %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "brief.md")); string(b) != "Build a gallery." {
		t.Errorf("brief.md = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "docs", "more", "a.md")); string(b) != "a\n" {
		t.Errorf("the copy of docs/more/a.md = %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "docs", "more", ".git")); err == nil {
		t.Error("a .git inside an input was copied into the workspace")
	}
	if b, _ := os.ReadFile(filepath.Join(r.Project, "docs", "input.md")); string(b) != "the input\n" {
		t.Errorf("the agent changed the project's docs/input.md through its copy: %q", b)
	}
	prompt := promptOf(t, r, "design-1")
	for _, want := range []string{"## Your workspace", "- docs/input.md is a copy", "- docs/more is a copy", "Write every document you produce under out/", "## Brief"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "missing.md") {
		t.Errorf("the prompt lists an input that is not in the project:\n%s", prompt)
	}
	if len(r.State.Leaves) != 0 {
		t.Errorf("a scratch node is not a leaf: leaves %v", r.State.Leaves)
	}
}

func TestEachAttemptOfAScratchAgentStartsFromAFreshWorkspace(t *testing.T) {
	nodes := scratchNode + "  - { id: design-gate, kind: gate, gate: design }\n"
	r := start(t, "key-points", "", nodes, designScript)
	withInputs(t, r)
	ctx := context.Background()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Reject(ctx, r, "design-gate", "shorter, please"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() after the rejection = %+v, %v", out, err)
	}
	ws := r.ScratchDir("design")
	if _, err := os.Stat(filepath.Join(ws, "out", "docs", "design.md")); err == nil {
		t.Error("the first attempt's output was still in the workspace for the second")
	}
	if _, err := os.Stat(filepath.Join(ws, "out", "docs", "other.md")); err != nil {
		t.Errorf("the second attempt's output is missing: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "docs", "input.md")); string(b) != "the input\n" {
		t.Errorf("the second attempt got %q for docs/input.md; the first attempt's change in its copy must not carry over", b)
	}
	if got := promptOf(t, r, "design-2"); !strings.Contains(got, "shorter, please") {
		t.Errorf("the second prompt lacks the rejection:\n%s", got)
	}
}

func TestAnInputThatCouldLeadOutOfTheProjectIsRefused(t *testing.T) {
	r := start(t, "key-points", "", scratchNode, designScript)
	withInputs(t, r)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("secret\n"), 0o644)
	if err := os.Symlink(secret, filepath.Join(r.Project, "docs", "more", "link.md")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "link.md") || !strings.Contains(out.Err.Error(), "not a regular file") {
		t.Errorf("Advance() = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(r.ScratchDir("design"), "docs", "more", "link.md")); err == nil {
		t.Error("the symlink's target was copied into the workspace")
	}
	if r.State.Attempts["design"] != 0 {
		t.Error("the agent ran although its workspace could not be made")
	}
}

func TestAScratchAgentRunsInAScratchSandboxWithNoGitDir(t *testing.T) {
	r, fake := startSandboxedWith(t, strings.Replace(scratchNode, "inputs: [docs/input.md, docs/more, missing.md] ", "", 1),
		"steps:\n  design:\n    - actions:\n        - write: { path: out/design.md, content: \"# Design\\n\" }\n        - run: { argv: [sh, -c, \"echo ok > ran-in-sandbox.txt\"] }\n      result: { status: ok, summary: wrote }\n", run.Hooks{})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if len(fake.Ups) != 1 {
		t.Fatalf("%d sandboxes came up", len(fake.Ups))
	}
	up := fake.Ups[0]
	if !up.Scratch || up.GitDir != "" || up.Name != "scratch/design" || up.Leaf != r.ScratchDir("design") || up.Parent != r.Dir {
		t.Errorf("spec = %+v", up)
	}
	if err := up.Validate(); err != nil {
		t.Errorf("the spec Odin built is one the driver would refuse: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(r.ScratchDir("design"), "ran-in-sandbox.txt")); err != nil || strings.TrimSpace(string(b)) != "ok" {
		t.Errorf("the agent's command did not run in the workspace: %q, %v", b, err)
	}
	if len(fake.Downs) != 1 || len(r.State.Sandboxes) != 0 {
		t.Errorf("downs %d, recorded %v; the scratch sandbox stops with the rest", len(fake.Downs), r.State.Sandboxes)
	}
	if fake.Scans != 0 {
		t.Errorf("%d secret scans ran for a node that commits nothing", fake.Scans)
	}
}

func TestARetryOfAScratchAgentReseedsTheWorkspaceItsSandboxMounts(t *testing.T) {
	// The scratch sandbox mounts the workspace folder and stays up across tries: the retry must find its
	// fresh copy in that same folder, not in a new one the container doesn't see (review I-3).
	script := "steps:\n  design:\n" +
		"    - actions:\n        - write: { path: out/first.md, content: \"# First\\n\" }\n        - run: { argv: [sh, -c, \"echo 1 > try.txt\"] }\n      error: the CLI crashed\n      kind: transient\n" +
		"    - actions:\n        - run: { argv: [sh, -c, \"echo 2 > try.txt\"] }\n        - write: { path: out/design.md, content: \"# Design\\n\" }\n      result: { status: ok, summary: wrote }\n"
	w := &waits{}
	h := hooks(w)
	r, fake := startSandboxedWith(t, strings.Replace(scratchNode, "inputs: [docs/input.md, docs/more, missing.md] ", "", 1), script, h)
	// The first try holds the folder open, as a container's mount does, so the folder it saw can't go and
	// come back under the same inode: each later command must run in that same folder.
	var mounted *os.File
	var same []bool
	fake.Intercept = func(c gungnir.Cmd) (gungnir.ExecResult, bool) {
		now, err := os.Stat(r.ScratchDir("design"))
		if err != nil {
			t.Errorf("the workspace is gone while its sandbox runs: %v", err)
			return gungnir.ExecResult{}, false
		}
		if mounted == nil {
			if mounted, err = os.Open(r.ScratchDir("design")); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { mounted.Close() })
		}
		was, err := mounted.Stat()
		if err != nil {
			t.Fatal(err)
		}
		same = append(same, os.SameFile(was, now))
		return gungnir.ExecResult{}, false
	}
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone || len(w.got) != 1 {
		t.Fatalf("Advance() = %+v, %v after %d waits; want done after one retry", out, err, len(w.got))
	}
	if len(fake.Ups) != 1 {
		t.Fatalf("%d sandboxes came up; the retry runs in the one the first try started", len(fake.Ups))
	}
	if len(same) != 2 || !same[0] || !same[1] {
		t.Errorf("the commands ran in the folder the sandbox mounts: %v; a retry must reseed that folder, not make another", same)
	}
	ws := r.ScratchDir("design")
	if _, err := os.Stat(filepath.Join(ws, "out", "first.md")); err == nil {
		t.Error("the failed try's output was still in the workspace for the retry")
	}
	if b, err := os.ReadFile(filepath.Join(ws, "out", "design.md")); err != nil || string(b) != "# Design\n" {
		t.Errorf("out/design.md = %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "try.txt")); strings.TrimSpace(string(b)) != "2" {
		t.Errorf("try.txt = %q", b)
	}
}

// An input that holds the project's root, such as ".", never brings the project's .factory (run state,
// ledgers, other runs' transcripts) or .git into a workspace: the workspace is the project as the agent
// may read it (review M-4). Gungnir refuses "." as an input; this holds whatever an input names.
func TestAnInputThatHoldsTheProjectRootLeavesOutFactoryAndGit(t *testing.T) {
	r := start(t, "key-points", "", scratchNode, designScript)
	withInputs(t, r)
	if _, err := os.Stat(filepath.Join(r.Project, ".factory")); err != nil {
		t.Fatalf("the project has no .factory to leave out: %v", err)
	}
	for _, in := range []string{".", "docs/.."} {
		n := schema.Node{ID: "design", Role: "planner", Prompt: "p.md", Scratch: true, Inputs: []string{in}}
		present, err := seedScratch(r, n)
		if err != nil {
			t.Fatalf("seedScratch(%q) = %v", in, err)
		}
		ws := filepath.Join(r.ScratchDir("design"), filepath.FromSlash(in))
		for _, hidden := range []string{".factory", ".git"} {
			if _, err := os.Lstat(filepath.Join(ws, hidden)); err == nil {
				t.Errorf("input %q: the workspace holds the project's %s", in, hidden)
			}
		}
		if b, err := os.ReadFile(filepath.Join(ws, "docs", "input.md")); err != nil || string(b) != "the input\n" {
			t.Errorf("input %q: the copy of docs/input.md = %q, %v", in, b, err)
		}
		if len(present) != 1 {
			t.Errorf("input %q: present = %v", in, present)
		}
	}
}
