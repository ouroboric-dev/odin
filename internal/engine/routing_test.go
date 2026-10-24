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

// goModScript makes the leaf require a package, so the wiring has an edge to build.
const goModScript = `steps:
  a:
    - actions:
        - write: { path: out.txt, content: "done\n" }
        - write: { path: go.mod, content: "module example.test/app\n\ngo 1.26\n\nrequire github.com/go-chi/chi/v5 v5.2.3\n" }
      result: { status: ok, summary: wrote out.txt, usage: { in: 100, out: 10, cache: 5, turns: 2, cost_usd: 0.5 }, output: '{"summary": "done"}' }
`

const shipNodes = build + "  - { id: merge, kind: merge, repo: repos/app, checks: [unit] }\n" +
	"  - { id: move, kind: pointer, repo: repos/app, generated: [REPOS.md, .ygg/impact.json] }\n"

func TestAPointerMoveShipsBothIndexesAndLogsThem(t *testing.T) {
	r := startPack(t, "key-points", "", packChecks, shipNodes, goModScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if files, _ := git.Run(bg, r.Project, "show", "--name-only", "--format=", "HEAD"); files != ".ygg/impact.json\nREPOS.md\nrepos/app" {
		t.Errorf("the move commits %q, want both indexes with the pointer", files)
	}
	repos, _ := git.Run(bg, r.Project, "show", "HEAD:REPOS.md")
	if !strings.Contains(repos, "| repos/app | requires | go:github.com/go-chi/chi/v5 | extracted | repos/app/go.mod |") {
		t.Errorf("REPOS.md lacks the edge:\n%s", repos)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"routing_index_built"`, `"ygg.routing.kind":"wiring","ygg.routing.edges":1,"ygg.routing.extracted":1`,
		`"ygg.routing.kind":"impact"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s", want)
		}
	}
}

// Review focus: an index that lost more than half of its edges against the last shipped one stops the
// move at the hard approval point shrink-guard, in every mode.
func TestAnIndexThatLostEdgesStopsAtTheShrinkGuard(t *testing.T) {
	r := startPack(t, "unattended", "", packChecks, shipNodes, goModScript)
	var es []routing.Edge
	for _, to := range []string{"a", "b", "c", "d", "e", "f"} {
		es = append(es, routing.Edge{From: "repos/app", To: "go:" + to, Kind: "requires", Tag: routing.Extracted, Source: "repos/app/go.mod"})
	}
	if err := routing.WriteWiring(r.Project, es); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "REPOS.md"}, {"commit", "-q", "-m", "an index of six edges"}} {
		if _, err := git.Run(bg, r.Project, args...); err != nil {
			t.Fatal(err)
		}
	}
	headBefore, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	linkBefore, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v (pending %s); want a stop at shrink-guard", out, err, r.State.PendingGate)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "move-1.md")); !strings.Contains(string(b), "REPOS.md keeps 1 of its 6 rows") {
		t.Errorf("gate file = %q", b)
	}
	// The refusal means the move did not complete: no commit, the pointer where it was, the old index kept.
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved from %s to %s", headBefore, got)
	}
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got != linkBefore {
		t.Errorf("the pointer moved from %q to %q", linkBefore, got)
	}
	committed, _ := git.Run(bg, r.Project, "show", "HEAD:REPOS.md")
	if rows, found := routing.WiringRows([]byte(committed)); !found || rows != 6 {
		t.Errorf("the committed REPOS.md has %d wiring rows (block found %v), want the old 6", rows, found)
	}
}

const benchPack = packChecks + "routing:\n  threshold: 0.8\n"

func writeBench(t *testing.T, r *run.Run, body string) {
	t.Helper()
	p := filepath.Join(r.Project, ".ygg", "routing-bench.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("schema: ygg.routing-bench/v1\nquestions:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const rightBench = "  - { id: who-requires-chi, kind: pin, about: repos/app, arg: \"go:github.com/go-chi/chi/v5\", answer: []}\n" +
	"  - { id: who-consumes-app, kind: consumers, about: repos/app, answer: [] }\n"

func TestAnIndexAtTheThresholdShipsAndTheLedgerKeepsTheScore(t *testing.T) {
	r := startPack(t, "key-points", "", benchPack, shipNodes, goModScript)
	writeBench(t, r, rightBench)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, "bench 2/2 right first at threshold 0.80") {
		t.Errorf("the ledger lacks the bench score:\n%s", text)
	}
}

// Review focus: an index under the pack's threshold does not ship. Nothing is written and the pointer stays.
func TestAnIndexUnderTheThresholdDoesNotShip(t *testing.T) {
	cases := map[string]string{
		"a wrong answer":  rightBench + "  - { id: who-consumes-nothing, kind: consumers, about: repos/app, answer: [ghost] }\n",
		"no bench at all": "",
	}
	for name, body := range cases {
		r := startPack(t, "key-points", "", benchPack, shipNodes, goModScript)
		if body != "" {
			writeBench(t, r, body)
		}
		before, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app")
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "does not ship") {
			t.Fatalf("%s: Advance() = %+v, %v; want a failed node that says the index does not ship", name, out, err)
		}
		if _, err := os.Stat(filepath.Join(r.Project, "REPOS.md")); err == nil {
			t.Errorf("%s: REPOS.md was written", name)
		}
		if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); after != before {
			t.Errorf("%s: the pointer moved", name)
		}
	}
}

func TestAPackWithoutARoutingBlockShipsUnmeasured(t *testing.T) {
	r := startPack(t, "key-points", "", packChecks, shipNodes, goModScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
}

// The bench is data in the project: a link where it should be is not followed, and the index does not ship.
func TestAnIndexDoesNotShipOnABenchThatIsALink(t *testing.T) {
	r := startPack(t, "key-points", "", benchPack, shipNodes, goModScript)
	elsewhere := filepath.Join(t.TempDir(), "bench.yaml")
	if err := os.WriteFile(elsewhere, []byte("schema: ygg.routing-bench/v1\nquestions:\n"+rightBench), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.Project, ".ygg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(r.Project, ".ygg", "routing-bench.yaml")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "does not ship") {
		t.Fatalf("Advance() = %+v, %v; want a failed node", out, err)
	}
	if _, err := os.Stat(filepath.Join(r.Project, "REPOS.md")); err == nil {
		t.Error("REPOS.md was written")
	}
}
