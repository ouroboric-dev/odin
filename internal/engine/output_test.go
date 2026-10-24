package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// outputNode is an agent in the leaf whose session returns a ygg.implement/v1 document.
const outputNode = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"

// answers is a script of node a whose sessions end with each of the given steps' result fields, in turn.
func answers(submits bool, results ...string) string {
	s := "steps:\n  a:\n"
	if submits {
		s = "submits: true\n" + s
	}
	for _, res := range results {
		s += "    - result: { status: ok, summary: answered, " + res + " }\n"
	}
	return s
}

// kept is the document Odin kept as node's output, and its record in run.json.
func kept(t *testing.T, r *run.Run, node string) (string, run.OutputRef) {
	t.Helper()
	ref, ok := r.State.Outputs[node]
	if !ok {
		t.Fatalf("run.json keeps no output of %s: %v", node, r.State.Outputs)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, "outputs", node+".json"))
	if err != nil {
		t.Fatalf("the output of %s is not in outputs/: %v", node, err)
	}
	return string(b), ref
}

func TestAnOutputComesThroughSubmitOrAsTheWholeAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		script, want string
	}{
		"through submit": {answers(true, `submitted: '{"summary": "through submit"}', output: "I called submit."`),
			`{"summary": "through submit"}`},
		"as the whole answer": {answers(false, "output: '{\"summary\": \"the whole answer\"}'"),
			`{"summary": "the whole answer"}`},
		"in one fence": {answers(false, "output: \"```json\\n{\\\"summary\\\": \\\"in a fence\\\"}\\n```\""),
			`{"summary": "in a fence"}`},
		"from a submitter that submitted nothing": {answers(true, "output: '{\"summary\": \"no submit\"}'"),
			`{"summary": "no submit"}`},
	} {
		r := start(t, "key-points", "", outputNode, c.script)
		if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
			t.Fatalf("%s: Advance() = %+v, %v", name, out, err)
		}
		doc, ref := kept(t, r, "a")
		if doc != c.want {
			t.Errorf("%s: outputs/a.json = %s, want %s", name, doc, c.want)
		}
		if ref.Contract != string(schema.ContractImplement) || ref.Path != "outputs/a.json" || ref.Attempt != 1 || len(ref.SHA256) != 64 {
			t.Errorf("%s: run.json keeps %+v", name, ref)
		}
		got, err := loadOutput[schema.Implement](r, "a")
		var want schema.Implement
		json.Unmarshal([]byte(c.want), &want) //nolint:errcheck // the test's own JSON
		if err != nil || got.Summary != want.Summary {
			t.Errorf("%s: loadOutput = %+v, %v", name, got, err)
		}
		if text := ledgerText(t, r); strings.Contains(text, `"event":"output_refused"`) {
			t.Errorf("%s: a valid output was refused:\n%s", name, text)
		}
	}
}

func TestAnInvalidOutputIsAskedForOnceMore(t *testing.T) {
	const bad = `{"summary": "", "extra": 1}`
	r := start(t, "key-points", "", outputNode, answers(false, "output: '"+bad+"'", `output: '{"summary": "fixed"}'`))
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	var ve *schema.ValidationError
	if err := schema.Validate(schema.ContractImplement, []byte(bad)); !errors.As(err, &ve) || len(ve.Errors) < 2 {
		t.Fatalf("Validate(%s) = %v; the test needs two errors", bad, err)
	}
	second := promptOf(t, r, "a-2")
	if !strings.Contains(second, "## Your answer could not be used") || !strings.Contains(second, "submit") {
		t.Errorf("the second session was not told its answer could not be used:\n%s", second)
	}
	for _, line := range ve.Errors {
		if !strings.Contains(second, "- "+line) {
			t.Errorf("the second prompt lacks the error %q:\n%s", line, second)
		}
	}
	if strings.Contains(promptOf(t, r, "a-1"), "could not be used") {
		t.Error("the first session was told of errors it had not made")
	}
	text := ledgerText(t, r)
	refused, repaired := strings.Index(text, `"event":"output_refused"`), strings.Index(text, `"event":"output_repaired"`)
	if refused < 0 || repaired < refused {
		t.Errorf("the ledger lacks output_refused, then output_repaired:\n%s", text)
	}
	if doc, ref := kept(t, r, "a"); doc != `{"summary": "fixed"}` || ref.Attempt != 2 {
		t.Errorf("kept %s as %+v; want the repaired answer of attempt 2", doc, ref)
	}
}

func TestTwoInvalidOutputsFailTheNode(t *testing.T) {
	r := start(t, "key-points", "", outputNode, answers(false, `output: '{"summary": ""}'`, `output: '{"summary": ""}'`))
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want a failed run", out, err)
	}
	if msg := out.Err.Error(); !strings.Contains(msg, "the output of a is not a valid ygg.implement/v1, after being asked twice") || !strings.Contains(msg, "/summary") {
		t.Errorf("the failure says %q", msg)
	}
	if _, ok := r.State.Outputs["a"]; ok {
		t.Error("run.json keeps an output that never validated")
	}
	if r.State.Attempts["a"] != 2 {
		t.Errorf("a ran %d sessions; want 2: one, and one repair", r.State.Attempts["a"])
	}
	if text := ledgerText(t, r); strings.Contains(text, `"event":"output_repaired"`) {
		t.Errorf("the ledger says repaired:\n%s", text)
	}
}

func TestAProseAnswerIsNeverRead(t *testing.T) {
	prose := `output: "Here is my answer.\n{\"summary\": \"inside prose\"}\nThat is all."`
	r := start(t, "key-points", "", outputNode, answers(false, prose, prose))
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; a valid document inside prose is never read", out, err)
	}
	if _, ok := r.State.Outputs["a"]; ok {
		t.Error("a document was read out of prose")
	}
	if !strings.Contains(promptOf(t, r, "a-2"), "exactly one JSON document") {
		t.Errorf("the repair session was not told why:\n%s", promptOf(t, r, "a-2"))
	}
}

func TestAKeptOutputThatChangedIsNotLoaded(t *testing.T) {
	r := start(t, "key-points", "", outputNode, answers(false, `output: '{"summary": "kept"}'`))
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "outputs", "a.json"), []byte(`{"summary": "changed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOutput[schema.Implement](r, "a"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("loadOutput of a changed file = %v", err)
	}
	if _, err := loadOutput[schema.Implement](r, "nobody"); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("loadOutput of a node with no output = %v", err)
	}
}

// A run started before typed boundaries keeps no outputs and was read with a schema 1 graph: its prompts and
// answers were prose, so it is refused, before anything else runs (review focus 5).
func TestAnOldRunIsNotResumed(t *testing.T) {
	r := start(t, "step-by-step", "", outputNode+"  - { id: design-gate, kind: gate, gate: design }\n",
		answers(false, `output: '{"summary": "done"}'`))
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	id, project, graphDir := r.State.ID, r.Project, r.State.GraphDir
	r.Close()
	var state map[string]any
	b, _ := os.ReadFile(filepath.Join(run.Dir(project, id), "run.json"))
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "outputs")
	b, _ = json.Marshal(state)
	if err := os.WriteFile(filepath.Join(run.Dir(project, id), "run.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := "name: test\nversion: 1.0.0\nschema: 1\nnodes:\n  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app }\n"
	if err := os.WriteFile(filepath.Join(graphDir, "graph.yaml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(run.Dir(project, id), "ledger.jsonl"))
	_, err := run.Open(bg, project, id, "")
	if err == nil || err.Error() != "run "+id+" was started before typed boundaries; start a new run" {
		t.Fatalf("Open() = %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(run.Dir(project, id), "ledger.jsonl")); string(after) != string(before) {
		t.Error("the refused run's ledger changed")
	}
}
