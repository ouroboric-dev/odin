package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

func TestTheRecordHoldsTheJudgementsInTheFile(t *testing.T) {
	dir := newFake(t).done()
	if r := mustCollect(t, dir); len(r.Judgements) != 0 {
		t.Fatalf("a run nobody judged has judgements: %+v", r.Judgements)
	}
	js := []schema.Judgement{
		{Run: "r", Score: 7, Well: "a", Wrong: "b", Time: "2026-10-25T09:30:00Z"},
		{Run: "r", Task: "t1", Score: 2, Well: "c", Wrong: "d", Time: "2026-10-25T09:31:00Z"},
	}
	if err := WriteJudgements(dir, js); err != nil {
		t.Fatal(err)
	}
	r := mustCollect(t, dir)
	if len(r.Judgements) != 2 || r.Judgements[0] != js[0] || r.Judgements[1] != js[1] {
		t.Errorf("judgements = %+v, want %+v", r.Judgements, js)
	}
	if _, err := Encode(r); err != nil {
		t.Errorf("a record with judgements does not encode: %v", err)
	}
	// A file that does not decode is a warning, and the record stands without it.
	if err := os.WriteFile(filepath.Join(dir, JudgementsFile), []byte(`[{"run":"r","score":11}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	r = mustCollect(t, dir)
	if len(r.Judgements) != 0 || !strings.Contains(strings.Join(r.Warnings, "\n"), "judgements") {
		t.Errorf("judgements = %+v, warnings = %v; want none, and a warning", r.Judgements, r.Warnings)
	}
}

// Every field of the record is in the retrospective's view or out of it on purpose (Objective): a field added to Run
// later fails here until it is put in one of the two lists.
func TestEveryFieldOfTheRecordIsInOrOutOfTheRetrosView(t *testing.T) {
	in := []string{"ID", "Status", "Mode", "Graph", "GraphVersion", "Factory", "ConfigSHA256", "BriefSHA256", "Roles", "Realms", "Time",
		"Usage", "ByRole", "Tasks", "Leaves", "BlockedTasks", "Retries",
		"Warnings", // without the warning about the judgements
		"Loops", "Review", "Findings", "Interventions",
		"Grade",        // the score's totals only
		"GradeSkipped", // in no record: it says why the text shows no grade
		"Routing", "Setup", "Harness"}
	out := []string{"Retro", "Judgements"}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[Run]()) {
		switch {
		case slices.Contains(in, f.Name) && slices.Contains(out, f.Name):
			t.Errorf("Run.%s is in both lists", f.Name)
		case !slices.Contains(in, f.Name) && !slices.Contains(out, f.Name):
			t.Errorf("Run.%s is in neither list: say whether the retrospective sees it, and make Objective agree", f.Name)
		}
	}
	r := &Run{Retro: json.RawMessage(`{"went_well": []}`), Judgements: []schema.Judgement{{Score: 5}}}
	o := reflect.ValueOf(*r.Objective())
	for _, name := range out {
		if !o.FieldByName(name).IsZero() {
			t.Errorf("Objective keeps Run.%s", name)
		}
	}
}
