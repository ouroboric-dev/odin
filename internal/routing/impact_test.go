package routing

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func fixtureImpact(t *testing.T) *Impact {
	t.Helper()
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	return ImpactOf(res)
}

func TestAChangeToAContractReachesItsOwnerAndItsReaders(t *testing.T) {
	imp := fixtureImpact(t)
	c, ok := imp.Contract("api/internal/api.openapi.yaml")
	if !ok {
		t.Fatal("the api's contract is not in the index")
	}
	if c.Owner != "api" || len(c.SHA256) != 64 {
		t.Errorf("contract = %+v", c)
	}
	if !slices.Equal(c.Readers.Extracted, []string{"web", "worker"}) || len(c.Readers.Inferred) != 0 {
		t.Errorf("readers = %+v, want web and worker, both extracted", c.Readers)
	}
	// The worker's copy of the contract is linked by title only: a change to that copy reaches nothing
	// by a fact, and the guess is reported on its own.
	w, ok := imp.Contract("worker/internal/api.openapi.yaml")
	if !ok || len(w.Readers.Extracted) != 0 {
		t.Errorf("worker's copy = %+v, %v", w, ok)
	}
}

func TestAChangeToALeafReachesWhatDependsOnIt(t *testing.T) {
	imp := fixtureImpact(t)
	r := imp.Reaches("api")
	if !slices.Equal(r.Extracted, []string{"web", "worker"}) {
		t.Errorf("api reaches %+v", r)
	}
	// worker is only guessed to be read by nobody; a change to web reaches worker only by the README guess.
	r = imp.Reaches("web")
	if len(r.Extracted) != 0 || !slices.Equal(r.Inferred, []string{"worker"}) {
		t.Errorf("web reaches %+v, want worker as a guess only", r)
	}
	if r := imp.Reaches("go:github.com/go-chi/chi/v5"); !slices.Equal(r.Extracted, []string{"api", "web", "worker"}) {
		t.Errorf("a bump of chi reaches %+v, want api and what depends on api", r)
	}
	if r := imp.Reaches("nobody"); len(r.Extracted)+len(r.Inferred) != 0 {
		t.Errorf("an unknown node reaches %+v", r)
	}
}

func TestTheImpactIndexIsALabelledLowerBoundAndReportsGuessesApart(t *testing.T) {
	imp := fixtureImpact(t)
	b, err := imp.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema"] != "ygg.impact/v1" || m["bound"] != "lower" {
		t.Errorf("schema %v, bound %v", m["schema"], m["bound"])
	}
	inferred, _ := m["inferred_edges"].([]any)
	if len(inferred) != 2 {
		t.Errorf("inferred_edges = %v, want the two guesses of the fixture", inferred)
	}
	if m["edges"].(float64) != 24 || m["extracted"].(float64) != 22 || m["inferred"].(float64) != 2 {
		t.Errorf("counts %v/%v/%v", m["edges"], m["extracted"], m["inferred"])
	}
}

func TestTheImpactIndexIsTheSameBytesEveryTimeAndRoundTrips(t *testing.T) {
	a, _ := fixtureImpact(t).JSON()
	b, _ := fixtureImpact(t).JSON()
	if !bytes.Equal(a, b) {
		t.Fatal("two runs gave two indexes")
	}
	golden := "../../testdata/routing/impact.golden.json"
	if *update {
		os.WriteFile(golden, a, 0o644)
	}
	if want, err := os.ReadFile(golden); err != nil || !bytes.Equal(want, a) {
		t.Errorf("the index differs from %s (%v):\n%s", golden, err, a)
	}
	dir := t.TempDir()
	res, _ := Wiring(fixture, packs())
	if err := WriteImpact(dir, ImpactOf(res)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadImpact(dir)
	if err != nil || got.Edges != 24 {
		t.Fatalf("ReadImpact = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ImpactFile)); err != nil {
		t.Error(err)
	}
}
