package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

const specAndReview = "  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.implement/v1 }\n" +
	"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec, input: [rejection], output: ygg.conformance/v1 }\n"

const specAndReviewScript = "steps:\n  spec:\n    - actions: [ { write: { path: out/docs/spec.md, content: \"# The spec\\n\" } } ]\n      result: { status: ok, summary: wrote the spec, output: '{\"summary\": \"the spec of the gallery\"}' }\n" +
	"  conformance:\n    - actions:\n        - run: { argv: [sh, -c, \"ls > out/seen.md\"] }\n" +
	"      result: { status: ok, summary: read the spec, output: '{\"deviation\": false}' }\n"

// A scratch reviewer reads the typed output of the node it reviews in its prompt: reviews: counts as an input
// (ruling R12), and nothing of that node's out/ is copied into its workspace.
func TestAScratchReviewerGetsTheOutputOfTheAgentItReviewsAsItsInput(t *testing.T) {
	r, _ := startSandboxedWith(t, specAndReview, specAndReviewScript, run.Hooks{})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(r.ScratchDir("conformance"), "out", "seen.md")); err != nil || strings.TrimSpace(string(b)) != "out" {
		t.Errorf("the reviewer's workspace holds %q, %v; want out/ alone", b, err)
	}
	prompt := promptOf(t, r, "conformance-1")
	if h := inputHeadings(prompt); len(h) != 2 || h[0] != "rejection" || h[1] != "spec" {
		t.Errorf("the reviewer's inputs are %v, want [rejection spec]:\n%s", h, prompt)
	}
	if !strings.Contains(prompt, "## Input: spec\n\n### summary\n\nthe spec of the gallery\n") {
		t.Errorf("the prompt lacks the reviewed output:\n%s", prompt)
	}
	if strings.Contains(prompt, "reviewed/") || strings.Contains(promptOf(t, r, "spec-1"), "## Input: spec") {
		t.Error("a prompt names a reviewed/ folder, or the reviewed node got its own output")
	}
}

// What the reviewed node left in its out/, a symlink too, never reaches the reviewer's workspace: nothing is
// copied there (publish refuses the link when out/ leaves the workspace).
func TestNothingOfTheReviewedOutputReachesTheReviewersWorkspace(t *testing.T) {
	script := "steps:\n  spec:\n    - actions:\n        - write: { path: out/docs/spec.md, content: \"# The spec\\n\" }\n        - run: { argv: [ln, -s, /etc/passwd, out/docs/link.md] }\n" +
		"      result: { status: ok, summary: wrote the spec, output: '{\"summary\": \"done\"}' }\n  conformance:\n    - result: { status: ok, summary: read it, output: '{\"deviation\": false}' }\n"
	r, _ := startSandboxedWith(t, specAndReview, script, run.Hooks{})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	entries, err := os.ReadDir(r.ScratchDir("conformance"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "out" {
		t.Errorf("the reviewer's workspace holds %v, %v; want out/ alone", entries, err)
	}
}
