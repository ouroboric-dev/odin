package metrics

import (
	"os"
	"path/filepath"
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
