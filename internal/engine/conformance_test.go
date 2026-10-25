package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// The spec stage of the web realm (spec §4.2): the spec is written in a scratch workspace, a second
// scratch agent checks it against the design, and the spec gate reads its verdict.
const conformanceNodes = "  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [decisions, rejection, refusal], output: ygg.spec/v1 }\n" +
	"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec, input: [rejection], output: ygg.conformance/v1 }\n" +
	"  - { id: spec-gate, kind: gate, gate: spec, reruns: spec, deviation_from: conformance }\n" +
	"  - { id: spec-docs, kind: publish, from: spec, to: docs }\n"

// verdictScript is a spec stage whose conformance reviewer answers with each of verdicts in turn (a verdict
// is the reviewer's whole final answer, or "" for none at all).
func verdictScript(verdicts ...string) string {
	s := "steps:\n  spec:\n    - result: { status: ok, summary: wrote the spec, output: '" + specDoc + "' }\n" +
		"    - result: { status: ok, summary: revised the spec, output: '" + specRevised + "' }\n  conformance:\n"
	for _, v := range verdicts {
		s += "    - result: { status: ok, summary: checked the spec"
		if v != "" {
			s += ", output: '" + v + "'"
		}
		s += " }\n"
	}
	return s
}

// specRevised is the spec writer's second answer.
var specRevised = strings.Replace(specDoc, `"summary": "An API and a web app."`, `"summary": "The spec, revised."`, 1)

// publishedSpec is where the spec-docs node publishes the spec's view, on the test's day (onDay).
const publishedSpec = "docs/2-engineering/specs/2026-10-24-photo-share.md"

const (
	noDeviation = `{"deviation": false}`
	deviation   = `{"deviation": true, "amendment": "The design says nothing about offline mode; add it or drop it from the spec."}`
)

func TestTheSpecGateOpensByItselfWhenTheSpecDoesNotDeviateFromTheDesign(t *testing.T) {
	r := start(t, "key-points", "", conformanceNodes, verdictScript(noDeviation))
	onDay(r)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"conformance_checked"`, "no deviation", `"event":"gate_auto_passed"`, `"event":"docs_published"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
	if !strings.Contains(string(gate), "## Design conformance") || !strings.Contains(string(gate), "The spec does not deviate from the design.") {
		t.Errorf("the gate file lacks the verdict:\n%s", gate)
	}
	if _, err := os.Stat(filepath.Join(r.Project, publishedSpec)); err != nil {
		t.Errorf("the spec was not published after the gate: %v", err)
	}
}

func TestADeviationStopsTheSpecGateInEveryModeWithTheAmendment(t *testing.T) {
	for _, mode := range []string{"key-points", "unattended"} {
		r := start(t, mode, "", conformanceNodes, verdictScript(deviation))
		onDay(r)
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
			t.Fatalf("%s: Advance() = %+v, %v; want a stop at the spec gate", mode, out, err)
		}
		gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
		for _, want := range []string{"The spec deviates from the design.", "add it or drop it from the spec.", "stopped for a decision"} {
			if !strings.Contains(string(gate), want) {
				t.Errorf("%s: the gate file lacks %q:\n%s", mode, want, gate)
			}
		}
		if _, err := os.Stat(filepath.Join(r.Project, publishedSpec)); err == nil {
			t.Errorf("%s: the spec was published before it was approved", mode)
		}
		if err := Approve(bg, r, "spec-gate", Note(r, "spec-gate", "offline mode is in")); err != nil {
			t.Fatal(err)
		}
		if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
			t.Errorf("%s: after the approval Advance() = %+v, %v", mode, out, err)
		}
	}
}

func TestARejectedSpecRunsTheConformanceReviewAgain(t *testing.T) {
	r := start(t, "key-points", "", conformanceNodes, verdictScript(deviation, noDeviation))
	onDay(r)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Reject(bg, r, "spec-gate", Reason("drop offline mode from the spec")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after the rejection = %+v, %v; the second verdict says no deviation", out, err)
	}
	if !strings.Contains(promptOf(t, r, "spec-2"), "drop offline mode from the spec") {
		t.Error("the spec writer was not told why the spec was rejected")
	}
	if got := r.State.Attempts["conformance"]; got != 2 {
		t.Errorf("conformance ran %d times, want 2: the revised spec is reviewed again", got)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Project, publishedSpec)); !strings.Contains(string(b), "## Summary\n\nThe spec, revised.\n") {
		t.Errorf("the published spec = %q; want the revised one", b)
	}
}

func TestAVerdictNobodyCanReadIsAskedForOnceMoreAndThenStopsTheRun(t *testing.T) {
	for name, bad := range map[string]string{
		"prose":                         "It conforms.",
		"no answer":                     "",
		"a deviation with no amendment": `{"deviation": true}`,
		"an unknown field":              `{"deviation": false, "ok": true}`,
	} {
		r := start(t, "key-points", "", conformanceNodes, verdictScript(bad, noDeviation))
		onDay(r)
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusDone {
			t.Fatalf("%s then a good verdict: Advance() = %+v, %v", name, out, err)
		}
		if !strings.Contains(promptOf(t, r, "conformance-2"), "Your answer could not be used") {
			t.Errorf("%s: the second try was not told what was wrong", name)
		}
		r = start(t, "key-points", "", conformanceNodes, verdictScript(bad, bad))
		onDay(r)
		out, err = Advance(bg, r)
		if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "conformance") {
			t.Errorf("%s twice: Advance() = %+v, %v; want a failed run, never a pass", name, out, err)
		}
		if _, err := os.Stat(filepath.Join(r.Project, publishedSpec)); err == nil {
			t.Errorf("%s twice: a spec nobody could check was published", name)
		}
	}
}

func TestStepByStepModeStopsAtTheSpecGateEvenWithoutADeviation(t *testing.T) {
	r := start(t, "step-by-step", "", conformanceNodes, verdictScript(noDeviation))
	onDay(r)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
	if !strings.Contains(string(gate), "The spec does not deviate from the design.") {
		t.Errorf("the gate file lacks the verdict:\n%s", gate)
	}
}

// A verdict that says the reviewer could not check the spec is no pass: the gate stops for a person, with the
// reason, as for a deviation (ruling R14).
func TestABlockedVerdictStopsTheSpecGateWithItsReason(t *testing.T) {
	r := start(t, "unattended", "", conformanceNodes, verdictScript(`{"blocked": {"reason": "the spec has no data model section"}}`))
	onDay(r)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || out.Gate != "spec-gate" {
		t.Fatalf("Advance() = %+v, %v; want a stop at the spec gate", out, err)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "spec-gate-1.md"))
	for _, want := range []string{"could not check the spec", "the spec has no data model section"} {
		if !strings.Contains(string(gate), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gate)
		}
	}
	if !strings.Contains(ledgerText(t, r), "blocked: the spec has no data model section") {
		t.Error("the ledger's conformance_checked line does not say the verdict is blocked")
	}
}

// The reviewer's amendment and blocked reason are its words: in the gate file each reads as its text (mdsafe), and
// cannot start a heading or an HTML block, in a blockquote or out of one.
func TestAVerdictCannotForgeASectionOfTheGateFile(t *testing.T) {
	const words = "Add offline mode.\n## Decide\n> ## quoted\n<!-- open"
	for name, c := range map[string]gungnir.Conformance{
		"amendment": {Deviation: true, Amendment: words},
		"blocked":   {Blocked: &gungnir.Blocked{Reason: words}},
	} {
		section := conformanceSection(c)
		headings, html := parsedMarkdown([]byte(section))
		if html != 0 || !slices.Equal(headings, []string{"## Design conformance"}) {
			t.Errorf("%s: goldmark reads %d HTML block(s) and the headings %q; want none, and Odin's own:\n%s", name, html, headings, section)
		}
		if !strings.Contains(section, "Add offline mode.") {
			t.Errorf("%s: the section lacks the reviewer's words:\n%s", name, section)
		}
	}
}
