package metrics

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// keep writes doc as the kept output of session key, as the engine keeps it, and records it in run.json.
func (f *fake) keep(key string, c schema.Contract, doc string) *fake {
	f.t.Helper()
	dir := filepath.Join(f.dir, "outputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), []byte(doc), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if f.State.Outputs == nil {
		f.State.Outputs = map[string]run.OutputRef{}
	}
	f.State.Outputs[key] = run.OutputRef{Contract: string(c), Path: "outputs/" + key + ".json", SHA256: sum([]byte(doc)), Attempt: 1}
	return f
}

const design = `{"title": "Gallery", "slug": "gallery", "summary": "s", "goals": "g", "non_goals": "n", "success_criteria": "c",
  "context": "x", "product_at_user_level": "p", "decisions": [],
  "open_questions": [{"id": "q1", "question": "Auth?", "who_can_answer": "owner"}, {"id": "q2", "question": "Dark mode?", "who_can_answer": "owner"},
    {"id": "q3", "question": "Paging?", "who_can_answer": "owner"}],
  "notes": ["the brief names no auth"]}`

func TestTheRecordCountsRefusalsByRoleAndModel(t *testing.T) {
	f := newFake(t)
	f.State.Roles["planner"] = run.RoleRecord{Agent: "claude-code", Model: "opus"}
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	refused := func(node, role, adapter, model string, attempt int) gungnir.Event {
		e := session(node, role, adapter, model, 10, 1, 1, 0.01, false)
		e.Name, e.Attempt, e.Note = gungnir.EventOutputRefused, attempt, "/summary: missing"
		return e
	}
	repaired := func(node, role, adapter, model string, c schema.Contract) gungnir.Event {
		return gungnir.Event{Name: gungnir.EventOutputRepaired, Node: node, Attempt: 1, Role: role, Adapter: adapter, Model: model, Note: string(c)}
	}
	f.log(1, refused("design", "planner", "claude-code", "opus", 1))
	f.log(2, session("design", "planner", "claude-code", "opus", 10, 1, 1, 0.01, false))
	f.log(2, repaired("design", "planner", "claude-code", "opus", schema.ContractDesign))
	f.log(3, refused("gallery-t1-implement-quality", "reviewer", "opencode", "qwen", 1))
	f.log(3, session("gallery-t1-implement-quality", "reviewer", "opencode", "qwen", 10, 1, 1, 0.01, false))
	f.log(3, repaired("gallery-t1-implement-quality", "reviewer", "opencode", "qwen", schema.ContractReview))
	capped := refused("gallery-t1-implement-quality", "reviewer", "opencode", "qwen", 2)
	capped.Result, capped.Note = "the session reached its turn cap (20 turns) before handing back its output", NoteTurnLimit
	f.log(4, capped) // a session at its turn cap: a refusal, and a turn limit too
	f.log(5, session("gallery-t1-implement", "implementer", "claude-code", "sonnet", 10, 1, 1, 0.01, false))
	// A later line of the same key that names another role does not take the blocked answer from the session that wrote it.
	f.log(6, gungnir.Event{Name: gungnir.EventRetry, Node: "gallery-t1-implement", Role: "reviewer", Adapter: "opencode", Model: "qwen"})
	f.keep("design", schema.ContractDesign, design)
	f.keep("gallery-t1-implement", schema.ContractImplement, `{"blocked": {"reason": "the contract names no endpoint"}, "notes": ["one", "two"]}`)
	f.keep("gallery-t1-implement-quality", schema.ContractReview, `{"verdict": "pass", "findings": [], "notes": ["changed after Odin kept it"]}`)
	f.State.Outputs["gallery-t1-implement-quality"] = run.OutputRef{Contract: string(schema.ContractReview), Path: "outputs/gallery-t1-implement-quality.json", SHA256: strings.Repeat("0", 64)}
	f.State.Decisions = []run.GateDecisions{{Gate: "design-gate", Decisions: schema.Decisions{Answers: []schema.Answer{{Question: "q1", Text: "none"}}}}}
	r := mustCollect(t, f.done())
	h := r.Harness
	if want := map[string]int{"planner claude-code opus": 1, "reviewer opencode qwen": 2}; !reflect.DeepEqual(h.Refused, want) {
		t.Errorf("refused = %v, want %v", h.Refused, want)
	}
	if want := map[string]int{"reviewer opencode qwen": 1}; !reflect.DeepEqual(h.TurnLimited, want) {
		t.Errorf("turn limited = %v, want %v", h.TurnLimited, want)
	}
	for k, n := range h.TurnLimited {
		if n > h.Refused[k] {
			t.Errorf("%s: %d turn limits, above its %d refused outputs", k, n, h.Refused[k])
		}
	}
	if want := map[string]int{"planner claude-code opus": 1, "reviewer opencode qwen": 1}; !reflect.DeepEqual(h.Repaired, want) {
		t.Errorf("repaired = %v, want %v", h.Repaired, want)
	}
	if want := []HarnessItem{{Node: "gallery-t1-implement", Role: "implementer", Agent: "claude-code", Model: "sonnet", Text: "the contract names no endpoint"}}; !reflect.DeepEqual(h.Blocked, want) {
		t.Errorf("blocked = %+v, want %+v", h.Blocked, want)
	}
	want := []HarnessItem{{Node: "design", Role: "planner", Agent: "claude-code", Model: "opus", Text: "the brief names no auth"},
		{Node: "gallery-t1-implement", Role: "implementer", Agent: "claude-code", Model: "sonnet", Text: "one"},
		{Node: "gallery-t1-implement", Role: "implementer", Agent: "claude-code", Model: "sonnet", Text: "two"}}
	if !reflect.DeepEqual(h.Notes, want) {
		t.Errorf("notes = %+v, want %+v", h.Notes, want)
	}
	if h.QuestionsLeftToSpec != 2 {
		t.Errorf("questions left to the spec = %d, want 2 (q2 and q3)", h.QuestionsLeftToSpec)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "gallery-t1-implement-quality") {
		t.Errorf("warnings = %v, want one for the output that changed", r.Warnings)
	}
	if _, err := Encode(r); err != nil {
		t.Errorf("Encode() = %v", err)
	}
}

// A run with no session and no kept output still has a harness, with empty lists, not missing ones.
func TestAnEmptyHarnessHasItsLists(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	r := mustCollect(t, f.done())
	b, err := Encode(r)
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	for _, want := range []string{`"refused": {}`, `"turn_limited": {}`, `"repaired": {}`, `"blocked": []`, `"notes": []`, `"questions_left_to_spec": 0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the record lacks %s:\n%s", want, b)
		}
	}
}
