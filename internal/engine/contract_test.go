package engine

import (
	"fmt"
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
		s += "        - write: { path: \"" + f + "\", content: \"openapi: 3.0.3\\ninfo: { title: T, version: 2.0.0 }\\n\" }\n"
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

// A name git lists with a leading space is a different file. A trim of the -z list would map it to the
// listed contract, and a contract the index never heard of would pass as one nobody reads.
func TestAContractWithALeadingSpaceIsNotTheListedContract(t *testing.T) {
	noOasdiff(t)
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWriting(" api.openapi.yaml"))
	shipIndex(t, r, routing.Reached{}) // lists api.openapi.yaml with no readers: a change there would pass
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"ygg.pointer.evidence":"inferred","ygg.verdict":"escalate"`) {
		t.Errorf("the ledger lacks the escalation:\n%s", text)
	}
}

// The gate judges one commit. If main moves on before the pointer does (another run, a later merge), the
// pointer must not carry the unjudged change along.
func TestThePointerDoesNotMoveToACommitTheGateDidNotJudge(t *testing.T) {
	noOasdiff(t)
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWriting("api.openapi.yaml"))
	shipIndex(t, r, routing.Reached{Extracted: []string{"web"}})
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the gate", out, err)
	}
	judged := mainOf(t, r)
	l, err := r.Leaf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	later, err := l.Git(bg, "commit-tree", "main^{tree}", "-p", "main", "-m", "a later change")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Git(bg, "update-ref", "refs/heads/main", later); err != nil {
		t.Fatal(err)
	}
	// The leaf's checkout follows main, as it does when another run merged into the same leaf.
	if _, err := l.Git(bg, "checkout", "-q", "--detach", later); err != nil {
		t.Fatal(err)
	}
	if err := Approve(bg, r, "contract-gate", "the contract change is in the spec"); err != nil {
		t.Fatal(err)
	}
	out, _ := Advance(bg, r)
	if out.Status != run.StatusFailed {
		t.Fatalf("the run is %s, want failed: the pointer may have moved to a commit the gate did not judge", out.Status)
	}
	if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got == later || got == judged {
		t.Errorf("the pointer is at %s; it must not have moved", got)
	}
}

// shipFolderIndex ships an index that lists a contract in the given path of the leaf, with no readers.
func shipFolderIndex(t *testing.T, r *run.Run, contract string) {
	t.Helper()
	imp := &routing.Impact{Schema: routing.ImpactSchema, Bound: "lower", Contracts: []routing.ContractEntry{
		{ID: "contract:repos/app/" + contract, Path: "repos/app/" + contract, Owner: "repos/app", Readers: routing.Reached{Extracted: []string{}, Inferred: []string{}}}},
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

// scriptWritingRaw is a scripted agent that writes out.txt and the given files (path to content, escaped for YAML).
func scriptWritingRaw(files map[string]string) string {
	s := "steps:\n  a:\n    - actions:\n        - write: { path: out.txt, content: \"done\\n\" }\n"
	for f, c := range files {
		s += "        - write: { path: \"" + f + "\", content: \"" + c + "\" }\n"
	}
	return s + "      result: { status: ok, summary: wrote files, usage: { in: 1, out: 1, cache: 0, turns: 1, cost_usd: 0.01 } }\n"
}

func TestAYAMLFileInAContractsFolderStopsTheGateInEveryMode(t *testing.T) {
	noOasdiff(t)
	for _, mode := range []string{"step-by-step", "key-points", "unattended"} {
		{
			r := startPack(t, mode, "", packChecks, contractNodes, scriptWritingRaw(map[string]string{"sqlc.yaml": "version: 2\\n"}))
			shipFolderIndex(t, r, "api.openapi.yaml")
			out, err := Advance(bg, r)
			if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
				t.Fatalf("%s: Advance() = %+v, %v; want a stop at the gate", mode, out, err)
			}
			if got, _ := git.Run(bg, r.Project, "rev-parse", "HEAD:repos/app"); got == mainOf(t, r) {
				t.Errorf("%s: the pointer moved before the approval", mode)
			}
			b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "contract-gate-1.md"))
			for _, want := range []string{"repos/app/sqlc.yaml", "YAML or JSON"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("%s: the gate file lacks %q:\n%s", mode, want, b)
				}
			}
			if text := ledgerText(t, r); !strings.Contains(text, `"ygg.pointer.evidence":"inferred","ygg.verdict":"escalate"`) {
				t.Errorf("%s: the ledger lacks the escalation", mode)
			}
		}
	}
}

func TestTheFolderRuleSelectsByTheFoldersOfTheShippedIndex(t *testing.T) {
	noOasdiff(t)
	cases := []struct {
		name     string
		contract string
		files    map[string]string
		stop     bool
	}{
		{"a JSON file in the folder", "api.openapi.yaml", map[string]string{"schema.json": "{}"}, true},
		{"a name with a space and a non-ASCII byte", "api.openapi.yaml", map[string]string{"my sch\u00e9ma.yaml": "a: 1\\n"}, true},
		{"a markdown file in the folder", "api.openapi.yaml", map[string]string{"README.md": "text"}, false},
		{"a YAML file below a contract at the leaf's root", "api.openapi.yaml", map[string]string{"ci/ci.yml": "a: 1\\n"}, false},
		{"a YAML file below a contract in a subfolder", "contracts/api.openapi.yaml", map[string]string{"contracts/schemas/user.yaml": "a: 1\\n"}, true},
		{"a YAML file beside a contract in a subfolder", "contracts/api.openapi.yaml", map[string]string{"contracts/user.yaml": "a: 1\\n"}, true},
		{"a YAML file at the root of a leaf whose contract is in a subfolder", "contracts/api.openapi.yaml", map[string]string{"sqlc.yaml": "a: 1\\n"}, false},
	}
	for _, c := range cases {
		r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWritingRaw(c.files))
		shipFolderIndex(t, r, c.contract)
		out, err := Advance(bg, r)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if stopped := out.Status == run.StatusStopped && out.Gate == "contract-gate"; stopped != c.stop {
			t.Errorf("%s: Advance() = %+v; want stop %t", c.name, out, c.stop)
		}
	}
}

func TestASymlinkInAContractsFolderStopsTheGate(t *testing.T) {
	noOasdiff(t)
	for _, mode := range []string{"key-points", "unattended"} {
		r := startPack(t, mode, "", packChecks, contractNodes, scriptWriting())
		shipFolderIndex(t, r, "api.openapi.yaml")
		// A link to a target outside the leaf, committed to the leaf's main.
		l, err := r.Leaf("repos/app")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/hostname", filepath.Join(l.Dir(), "notes.txt")); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "notes.txt"}, {"commit", "-q", "-m", "a link"}} {
			if _, err := l.Git(bg, args...); err != nil {
				t.Fatal(err)
			}
		}
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
			t.Fatalf("%s: Advance() = %+v, %v; want a stop at the gate", mode, out, err)
		}
		b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "contract-gate-1.md"))
		if !strings.Contains(string(b), "repos/app/notes.txt") || !strings.Contains(string(b), "symlink") {
			t.Errorf("%s: the gate file lacks the link:\n%s", mode, b)
		}
	}
}

// An absent index lists no contract, so no folder is watched; a corrupt one escalates the whole move
// (TestACorruptShippedIndexEscalatesTheMove).
func TestNoIndexWatchesNoFolder(t *testing.T) {
	noOasdiff(t)
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWritingRaw(map[string]string{"sqlc.yaml": "a: 1\\n"}))
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want the run done", out, err)
	}
}

func TestMoreThanTheCapOfFolderFilesEscalates(t *testing.T) {
	noOasdiff(t)
	files := map[string]string{}
	for i := 0; i <= maxContractChanges; i++ {
		files[fmt.Sprintf("f%d.yaml", i)] = "a: 1\\n"
	}
	r := startPack(t, "unattended", "", packChecks, contractNodes, scriptWritingRaw(files))
	shipFolderIndex(t, r, "api.openapi.yaml")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "contract-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop", out, err)
	}
	if text := ledgerText(t, r); !strings.Contains(text, "more than") {
		t.Error("the ledger lacks the cap reason")
	}
}

func TestParseRawDiffKeepsNamesAndModes(t *testing.T) {
	raw := ":000000 100644 0000000 abc1234 A\x00my schéma.yaml\x00:120000 100644 abc1234 def5678 T\x00a/link\x00:100644 000000 abc1234 0000000 D\x00x.json\x00"
	got, err := parseRawDiff([]byte(raw))
	want := []rawChange{{"my schéma.yaml", "000000", "100644"}, {"a/link", "120000", "100644"}, {"x.json", "100644", "000000"}}
	if err != nil || len(got) != len(want) {
		t.Fatalf("parseRawDiff = %v, %v; want %v", got, err, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, err := parseRawDiff(nil); err != nil || len(got) != 0 {
		t.Errorf("an empty diff: %v, %v", got, err)
	}
	if _, err := parseRawDiff([]byte("garbage\x00file\x00")); err == nil {
		t.Error("a line that is not a raw header was read")
	}
}
