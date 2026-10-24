package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// The planning stages up to the spec gate: the design's gate takes the owner's decisions, the spec reads them, and
// the conformance review comes after the owner-decision check.
const ownerNodes = "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.design/v1 }\n" +
	"  - { id: design-gate, kind: gate, gate: design, reruns: design }\n" +
	"  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [design, decisions, rejection, refusal], output: ygg.spec/v1 }\n" +
	"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec, input: [spec], output: ygg.conformance/v1 }\n" +
	"  - { id: spec-gate, kind: gate, gate: spec, reruns: spec, deviation_from: conformance }\n"

// fiveDecisions is what the product owner gives at the design gate, under ids that stay the same in every run.
var fiveDecisions = schema.Decisions{Decisions: []schema.Decision{
	{ID: "password-reset", Text: "The reset link goes by email."},
	{ID: "storage-cap", Text: "Each family has 10 GB."},
	{ID: "guest-download", Text: "Guests may download photos."},
	{ID: "logout-on-change", Text: "Changing the password signs out every other session."},
	{ID: "album-limit", Text: "An album holds at most 500 photos."},
}}

// specCiting is the spec fixture with decisions and a deviation that cite these ids: the last one in the deviation,
// the rest in the decisions.
func specCiting(ids ...string) string {
	last := ids[len(ids)-1]
	cites := `"` + strings.Join(ids[:len(ids)-1], `", "`) + `"`
	return strings.Replace(specDoc, `"decisions": [], "deviations": []`,
		`"decisions": [{"date": "2026-10-24", "decision": "Albums use random ids.", "reason": "The owner asked.", "cites": [`+cites+`]}], `+
			`"deviations": [{"what": "The album cap.", "where": "Design, Scope", "why": "The owner capped albums.", "cites": ["`+last+`"]}]`, 1)
}

var (
	citesFour = specCiting("password-reset", "storage-cap", "guest-download", "album-limit")
	citesFive = specCiting("password-reset", "storage-cap", "guest-download", "logout-on-change", "album-limit")
)

// ownerScript is a designer, then a spec writer that answers with each spec in turn, and a reviewer that finds no
// deviation every time.
func ownerScript(specs ...string) string {
	s := "steps:\n  design:\n    - result: { status: ok, summary: designed, output: '" + designDoc + "' }\n  spec:\n"
	for _, sp := range specs {
		s += "    - result: { status: ok, summary: wrote the spec, output: '" + sp + "' }\n"
	}
	s += "  conformance:\n"
	for range specs {
		s += "    - result: { status: ok, summary: checked, output: '" + noDeviation + "' }\n"
	}
	return s
}

const wantRefusal = "the spec does not cite the product owner's decisions logout-on-change: cite each in a decision or a deviation"

func TestTheSpecMustCiteEveryOwnerDecision(t *testing.T) {
	// The check itself: the ids that no decision and no deviation cites, in the order the gates gave them.
	spec, err := schema.Decode[schema.Spec](schema.ContractSpec, []byte(citesFour))
	if err != nil {
		t.Fatal(err)
	}
	ds := []run.GateDecisions{{Gate: "design-gate", Decisions: fiveDecisions},
		{Gate: "spec-gate", Decisions: schema.Decisions{Decisions: []schema.Decision{{ID: "dark-mode", Text: "No dark mode."}, {ID: "logout-on-change", Text: "Again."}}}}}
	if got := checkOwnerDecisions(spec, ds); !slices.Equal(got, []string{"logout-on-change", "dark-mode"}) {
		t.Errorf("checkOwnerDecisions() = %q; want [logout-on-change dark-mode]: each id once, in the order given", got)
	}
	if got := checkOwnerDecisions(spec, ds[:1]); !slices.Equal(got, []string{"logout-on-change"}) {
		t.Errorf("checkOwnerDecisions() of the design gate's five = %q; want [logout-on-change]", got)
	}
	five, _ := schema.Decode[schema.Spec](schema.ContractSpec, []byte(citesFive))
	if got := checkOwnerDecisions(five, ds[:1]); len(got) != 0 {
		t.Errorf("checkOwnerDecisions() of a spec citing all five = %q; want none", got)
	}

	// A spec that cites four of the five goes back once with the one it misses, before the conformance review.
	r := start(t, "key-points", "", ownerNodes, ownerScript(citesFour, citesFive))
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", fiveDecisions); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done: the second spec cites all five", out, err)
	}
	if r.State.Attempts["spec"] != 2 || r.State.Attempts["conformance"] != 1 {
		t.Errorf("spec ran %d time(s) and conformance %d; want 2 and 1: the check comes before the review", r.State.Attempts["spec"], r.State.Attempts["conformance"])
	}
	if p := promptOf(t, r, "spec-2"); !strings.Contains(p, "## Input: refusal\n\n") || !strings.Contains(p, wantRefusal) {
		t.Errorf("the second spec's prompt lacks the refusal %q:\n%s", wantRefusal, p)
	}
	if p := promptOf(t, r, "spec-1"); strings.Contains(p, wantRefusal) {
		t.Errorf("the first spec's prompt holds a refusal:\n%s", p)
	}
	if l := ledgerText(t, r); !strings.Contains(l, `"ygg.node.id":"spec","event":"node_returned"`) || !strings.Contains(l, "logout-on-change") {
		t.Errorf("the ledger lacks the spec's return:\n%s", l)
	}

	// A spec that cites all five passes the first time.
	r = start(t, "key-points", "", ownerNodes, ownerScript(citesFive))
	advanceTo(t, r, "design-gate")
	if err := Approve(bg, r, "design-gate", fiveDecisions); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone || r.State.Attempts["spec"] != 1 {
		t.Fatalf("Advance() = %+v, %v, %d spec session(s); want done after one", out, err, r.State.Attempts["spec"])
	}

	// A spec that misses one twice stops the run at the spec gate, in every mode, with the id in the gate file.
	for _, mode := range []string{"key-points", "unattended"} {
		r = start(t, mode, "", ownerNodes, ownerScript(citesFour, citesFour))
		advanceTo(t, r, "design-gate")
		if err := Approve(bg, r, "design-gate", fiveDecisions); err != nil {
			t.Fatal(err)
		}
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
			t.Fatalf("%s: Advance() = %+v, %v; want a stop at spec-gate", mode, out, err)
		}
		if r.State.Attempts["spec"] != 2 {
			t.Errorf("%s: spec ran %d time(s); want 2: it goes back once", mode, r.State.Attempts["spec"])
		}
		gf, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
		if !strings.Contains(string(gf), "## Owner decisions the spec does not cite\n") || !strings.Contains(string(gf), "- logout-on-change\n") ||
			!strings.Contains(string(gf), "stopped for a decision") {
			t.Errorf("%s: the gate file lacks the id the spec does not cite:\n%s", mode, gf)
		}
		if err := Approve(bg, r, "spec-gate", schema.Decisions{}); err != nil {
			t.Fatal(err)
		}
		if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
			t.Errorf("%s: after the approval Advance() = %+v, %v", mode, out, err)
		}
	}
}

// The check sends the spec its refusal, and the spec must see the decisions it is to cite: a spec node that does
// not declare both could only fail the check, so the run does not start.
func TestASpecThatCannotSeeTheOwnerDecisionsIsRefusedWhenTheRunStarts(t *testing.T) {
	for _, input := range []string{"[design, rejection, refusal]", "[design, decisions, rejection]"} {
		nodes := strings.Replace(ownerNodes, "input: [design, decisions, rejection, refusal]", "input: "+input, 1)
		_, err := run.Start(bg, startOptions(t, setup{nodes: nodes, script: ownerScript(citesFive)}))
		if err == nil || !strings.Contains(err.Error(), "node spec: Odin checks that the spec cites every owner decision") ||
			!strings.Contains(err.Error(), "must declare the inputs decisions and refusal") {
			t.Errorf("input %s: Start() = %v; want the spec refused", input, err)
		}
	}
}
