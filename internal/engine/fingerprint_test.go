package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// editAgent runs the scripted agent, records each session's node, and after the session of node after it calls
// edit: a person changing a file while the run goes on.
type editAgent struct {
	gungnir.Agent
	after string
	edit  func()
	nodes *[]string
}

func (e editAgent) Run(ctx context.Context, t gungnir.Task) (gungnir.Result, error) {
	*e.nodes = append(*e.nodes, t.Node)
	res, err := e.Agent.Run(ctx, t)
	if t.Node == e.after {
		e.edit()
	}
	return res, err
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A run uses one set of instructions (ruling R56): a prompt changed between two sessions of one process fails the
// next session's node before that session starts, and says to start a new run.
func TestAPromptChangedDuringTheRunFailsTheNextSessionBeforeItStarts(t *testing.T) {
	script := "steps:\n  a:\n    - result: { status: ok, summary: a, output: '{\"summary\": \"a\"}' }\n" +
		"  b:\n    - result: { status: ok, summary: b, output: '{\"summary\": \"b\"}' }\n"
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n" +
		"  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	r := start(t, "unattended", "", nodes, script)
	prompt := filepath.Join(r.Setup.GraphPack.Dir, "p.md")
	before, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte("Do other work.\n")
	var ran []string
	r.Setup.Agents["planner"] = editAgent{Agent: r.Setup.Agents["planner"], after: "a", nodes: &ran, edit: func() {
		if err := os.WriteFile(prompt, changed, 0o644); err != nil {
			t.Error(err)
		}
	}}
	out, err := Advance(context.Background(), r)
	want := "the prompt p.md changed during the run (recorded sha256 " + hexSum(before)[:12] + "…, now " + hexSum(changed)[:12] + "…); start a new run"
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), want) {
		t.Fatalf("Advance() = %+v, %v; want the run failed at b with %q", out, err, want)
	}
	if strings.Join(ran, ",") != "a" {
		t.Errorf("sessions ran for %v; want a only: b's never starts", ran)
	}
	if !strings.Contains(r.State.Error, "node b: "+want) {
		t.Errorf("run.json error = %q", r.State.Error)
	}
}
