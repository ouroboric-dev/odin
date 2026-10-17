package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

const specAndReview = "  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true }\n" +
	"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec }\n"

const specAndReviewScript = "steps:\n  spec:\n    - actions: [ { write: { path: out/docs/spec.md, content: \"# The spec\\n\" } } ]\n      result: { status: ok, summary: wrote the spec }\n" +
	"  conformance:\n    - actions:\n        - run: { argv: [sh, -c, \"cat reviewed/docs/spec.md > out/seen.md; echo changed > reviewed/docs/spec.md\"] }\n" +
	"      result: { status: ok, summary: read the spec }\n"

func TestAScratchReviewerFindsTheOutputOfTheAgentItReviewsAsACopy(t *testing.T) {
	r, _ := startSandboxedWith(t, specAndReview, specAndReviewScript, run.Hooks{})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(r.ScratchDir("conformance"), "out", "seen.md")); err != nil || string(b) != "# The spec\n" {
		t.Errorf("the reviewer saw %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.ScratchDir("spec"), "out", "docs", "spec.md")); string(b) != "# The spec\n" {
		t.Errorf("the reviewer's change reached the output it reviewed: %q", b)
	}
	prompt := promptOf(t, r, "conformance-1")
	for _, want := range []string{"- reviewed/ holds the output of the node spec", "What you change in it is ignored"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(promptOf(t, r, "spec-1"), "reviewed/") {
		t.Error("a scratch agent that reviews nothing is told it has a reviewed/ folder")
	}
}

func TestAReviewerIsRefusedAnOutputThatCouldLeadOutOfTheRun(t *testing.T) {
	script := "steps:\n  spec:\n    - actions:\n        - write: { path: out/docs/spec.md, content: \"# The spec\\n\" }\n        - run: { argv: [ln, -s, /etc/passwd, out/docs/link.md] }\n" +
		"      result: { status: ok, summary: wrote the spec }\n  conformance:\n    - result: { status: ok, summary: read it }\n"
	r, _ := startSandboxedWith(t, specAndReview, script, run.Hooks{})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "link.md") {
		t.Fatalf("Advance() = %+v, %v; want the run failed on link.md", out, err)
	}
	if r.State.Attempts["conformance"] != 0 {
		t.Error("the reviewer ran although its workspace could not be made")
	}
	if _, err := os.Stat(filepath.Join(r.ScratchDir("conformance"), "reviewed", "docs", "link.md")); err == nil {
		t.Error("the symlink was copied into the reviewer's workspace")
	}
}
