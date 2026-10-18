package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/routing"
	"github.com/ouroboric-dev/odin/internal/run"
)

const contractNodes = build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
	"  - { id: contract-gate, kind: gate, gate: pointer-contract }\n" +
	"  - { id: move, kind: pointer, repo: repos/app }\n"

// scriptWriting is a scripted agent that writes out.txt and the given extra files into the leaf.
func scriptWriting(files ...string) string {
	s := "steps:\n  a:\n    - actions:\n        - write: { path: out.txt, content: \"done\\n\" }\n"
	for _, f := range files {
		s += "        - write: { path: " + f + ", content: \"openapi: 3.0.3\\ninfo: { title: T, version: 2.0.0 }\\n\" }\n"
	}
	return s + "      result: { status: ok, summary: wrote files, usage: { in: 1, out: 1, cache: 0, turns: 1, cost_usd: 0.01 } }\n"
}

// shipIndex commits an impact index to the project that lists one contract with the given readers.
func shipIndex(t *testing.T, r *run.Run, readers routing.Reached) {
	t.Helper()
	// The index is an exact schema: every list is present, as [] when empty.
	if readers.Extracted == nil {
		readers.Extracted = []string{}
	}
	if readers.Inferred == nil {
		readers.Inferred = []string{}
	}
	imp := &routing.Impact{Schema: routing.ImpactSchema, Bound: "lower", Contracts: []routing.ContractEntry{
		{ID: "contract:repos/app/api.openapi.yaml", Path: "repos/app/api.openapi.yaml", Owner: "repos/app", Readers: readers}},
		Reach: map[string]routing.Reached{}, InferredEdges: []routing.Edge{}}
	if err := routing.WriteImpact(r.Project, imp); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", routing.ImpactFile}, {"commit", "-q", "-m", "ship the impact index"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func noOasdiff(t *testing.T) {
	old := contractDiffer
	contractDiffer = nil
	t.Cleanup(func() { contractDiffer = old })
}

func TestAMoveThatChangesNoContractPassesTheGate(t *testing.T) {
	noOasdiff(t)
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWriting())
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"pointer_contract_checked"`, `"ygg.pointer.evidence":"extracted","ygg.verdict":"pass"`, `"event":"gate_auto_passed"`, `"event":"pointer_moved"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s", want)
		}
	}
}

func TestAMoveThatChangesAContractAnExtractedEdgeReachesStopsAtTheGate(t *testing.T) {
	noOasdiff(t)
	for _, mode := range []string{"key-points", "unattended"} {
		r := startPack(t, mode, "", packChecks, contractNodes, scriptWriting("api.openapi.yaml"))
		shipIndex(t, r, routing.Reached{Extracted: []string{"web"}})
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" || r.State.PendingGate != gungnir.GatePointerContract {
			t.Fatalf("%s: Advance() = %+v, %v; want a stop at the pointer-contract gate", mode, out, err)
		}
		if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got == mainOf(t, r) {
			t.Errorf("%s: the pointer moved before the approval", mode)
		}
		b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "contract-gate-1.md"))
		for _, want := range []string{"## Contract evidence", "repos/app/api.openapi.yaml", "[web] read by extracted edges"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: the gate file lacks %q:\n%s", mode, want, b)
			}
		}
		if text := ledgerText(t, r); !strings.Contains(text, `"ygg.pointer.evidence":"extracted","ygg.verdict":"fail"`) {
			t.Errorf("%s: the ledger doesn't record the failed check", mode)
		}
		if err := Approve(bg, r, "contract-gate", "the contract change is in the spec"); err != nil {
			t.Fatal(err)
		}
		if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
			t.Fatalf("%s: Advance() after approval = %+v, %v", mode, out, err)
		}
		if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got != mainOf(t, r) {
			t.Errorf("%s: the pointer did not move after the approval", mode)
		}
	}
}

// Review focus: a contract that only an inferred edge reaches is never a pass; it goes to a person.
func TestAContractOnlyAGuessReachesEscalatesInEveryMode(t *testing.T) {
	noOasdiff(t)
	for _, mode := range []string{"key-points", "unattended"} {
		r := startPack(t, mode, "", packChecks, contractNodes, scriptWriting("api.openapi.yaml"))
		shipIndex(t, r, routing.Reached{Inferred: []string{"worker"}})
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
			t.Fatalf("%s: Advance() = %+v, %v; want a stop", mode, out, err)
		}
		if text := ledgerText(t, r); !strings.Contains(text, `"ygg.pointer.evidence":"inferred","ygg.verdict":"escalate"`) {
			t.Errorf("%s: the ledger lacks the escalation", mode)
		}
	}
}

func mainOf(t *testing.T, r *run.Run) string {
	t.Helper()
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	main, err := l.Rev(bg, "main")
	if err != nil {
		t.Fatal(err)
	}
	return main
}

// An index that can't be read is data we can't judge by: the move goes to a person, never to a pass, even
// when it changes no contract.
func TestACorruptShippedIndexEscalatesTheMove(t *testing.T) {
	noOasdiff(t)
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWriting())
	if err := os.MkdirAll(filepath.Join(r.Project, ".ygg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Project, routing.ImpactFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", routing.ImpactFile}, {"commit", "-q", "-m", "ship a corrupt index"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"ygg.pointer.evidence":"inferred","ygg.verdict":"escalate"`) {
		t.Error("the ledger lacks the escalation")
	}
}
