package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/agents"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
)

const scratchNode = "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.implement/v1 }\n"

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
	// The project's folders are not copied in since graph schema 2: inputs are typed (typed-T16).
	if _, err := os.Stat(filepath.Join(ws, "docs", "more")); err == nil {
		t.Error("a project folder was copied into the workspace")
	}
	if b, _ := os.ReadFile(filepath.Join(r.Project, "docs", "input.md")); string(b) != "the input\n" {
		t.Errorf("the agent changed the project's docs/input.md: %q", b)
	}
	prompt := promptOf(t, r, "design-1")
	for _, want := range []string{"## Your workspace", "Write every document you produce under out/", "## Brief"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
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
	if _, err := os.Stat(filepath.Join(ws, "docs", "input.md")); err == nil {
		t.Error("the first attempt's docs/input.md was still in the workspace for the second")
	}
	if got := promptOf(t, r, "design-2"); !strings.Contains(got, "shorter, please") {
		t.Errorf("the second prompt lacks the rejection:\n%s", got)
	}
}

func TestAnInputThatCouldLeadOutOfTheProjectIsRefused(t *testing.T) {
	t.Skip("typed-T16: graph schema 2 has no inputs: folders to copy; T16 decides the workspace's input files")
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
	r, fake := startSandboxedWith(t, scratchNode,
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
	r, fake := startSandboxedWith(t, scratchNode, script, h)
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

const stackImage = "docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514"

func TestAScratchSandboxStartsOnTheFactoryLayerWithTheModelAPIsAsItsOnlyEgress(t *testing.T) {
	fake := &sandboxfake.Sandbox{}
	r := startRealms(t, realmsSetup{
		roles:  "roles:\n  planner: { agent: claude-code, model: sonnet }\n  implementer: { agent: scripted, model: none, script: script.yaml }\n",
		nodes:  scratchNode + build, // build names the leaf, so the run has one
		script: "steps: {}\n",
		realms: []realmFiles{
			{name: "software", version: "0.1.0", pack: "", files: map[string]string{"p.md": "Prompt.\n"}}, // a graph pack with no sandbox block
			{name: "leaf-web", version: "0.1.0", pack: "sandbox:\n  stack: " + stackImage + "\n  egress: [registry.npmjs.org, api.osv.dev]\n"},
		},
		leaves:  map[string]string{"repos/app": "leaf-web"},
		sandbox: fake,
	})
	if r.Setup.Sandbox == nil || r.Setup.Sandbox.Config.Images != sandbox.FactoryImages() {
		t.Fatalf("Sandbox = %+v; a graph pack with no sandbox block prepares with the factory images %+v", r.Setup.Sandbox, sandbox.FactoryImages())
	}
	if _, err := r.ScratchRunner(context.Background(), "design"); err != nil {
		t.Fatal(err)
	}
	var scratch, leaf *gungnir.SandboxSpec
	for i, s := range fake.Ups {
		switch s.Name {
		case "scratch/design":
			scratch = &fake.Ups[i]
		}
	}
	if scratch == nil {
		t.Fatalf("no scratch sandbox came up: %+v", fake.Ups)
	}
	if scratch.Stack != "" {
		t.Errorf("the scratch sandbox starts on the stack %q, want none: the factory layer alone", scratch.Stack)
	}
	want := slices.Clone(agents.Hosts("claude-code"))
	slices.Sort(want)
	if !slices.Equal(scratch.Egress, want) {
		t.Errorf("the scratch egress is %v, want only the model API hosts %v", scratch.Egress, want)
	}
	if len(scratch.Services) != 0 {
		t.Errorf("the scratch sandbox has services %v", scratch.Services)
	}
	if err := scratch.Validate(); err != nil {
		t.Errorf("the scratch spec is one the driver would refuse: %v", err)
	}
	// A leaf of the same run keeps its realm's registries and stack.
	if _, err := r.Runner(context.Background(), "repos/app"); err != nil {
		t.Fatal(err)
	}
	for i, s := range fake.Ups {
		if s.Name == "repos/app" {
			leaf = &fake.Ups[i]
		}
	}
	if leaf == nil || leaf.Stack != stackImage || !slices.Contains(leaf.Egress, "registry.npmjs.org") || !slices.Contains(leaf.Egress, "api.osv.dev") {
		t.Errorf("the leaf's sandbox is %+v, want its realm's stack and registries", leaf)
	}
}

func TestTheRoutingThresholdComesFromTheGraphPack(t *testing.T) {
	const nodes = build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
		"  - { id: move, kind: pointer, repo: repos/app, generated: [REPOS.md, .ygg/impact.json] }\n"
	const routingBlock = "routing:\n  threshold: 1.0\n"
	leafChecks := packChecks
	launch := func(graphPack, leafPack string) (*run.Run, error) {
		r, err := tryRealms(t, realmsSetup{nodes: nodes, script: goModScript,
			realms: []realmFiles{
				{name: "software", version: "0.1.0", pack: graphPack, files: map[string]string{"p.md": "Prompt.\n"}},
				{name: "leaf-app", version: "0.1.0", pack: leafPack, files: map[string]string{"p.md": "Prompt.\n"}},
			},
			leaves: map[string]string{"repos/app": "leaf-app"}})
		return r, err
	}
	// The threshold in the graph pack, and a bench the index misses: the move fails.
	r, err := launch(routingBlock, leafChecks)
	if err != nil {
		t.Fatal(err)
	}
	writeBench(t, r, rightBench+"  - { id: who-consumes-nothing, kind: consumers, about: repos/app, answer: [ghost] }\n")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "at threshold 1.00") {
		t.Fatalf("graph pack threshold: Advance() = %+v, %v; want a failed node that says the index does not ship", out, err)
	}
	// The same block in the leaf realm's pack only: nothing sets the bar, the index ships unmeasured.
	r, err = launch("", leafChecks+routingBlock)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("leaf realm threshold: Advance() = %+v, %v; want the index shipped unmeasured", out, err)
	}
}
