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
      result: { status: ok, summary: wrote out.txt, usage: { in: 100, out: 10, cache: 5, turns: 2, cost_usd: 0.5 } }
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
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateShrinkGuard {
		t.Fatalf("Advance() = %+v, %v (pending %s); want a stop at shrink-guard", out, err, r.State.PendingGate)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "move-1.md")); !strings.Contains(string(b), "REPOS.md keeps 1 of its 6 rows") {
		t.Errorf("gate file = %q", b)
	}
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got == "" {
		t.Error("no pointer")
	}
}
