package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const benchFile = "../../testdata/routing/bench.yaml"

func fixtureAnswerers(t *testing.T) (Answerer, Answerer, *Bench) {
	t.Helper()
	b, err := LoadBench(benchFile)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	return IndexAnswerer(res, ImpactOf(res)), GrepBaseline(fixture, res.Leaves), b
}

func TestTheIndexAnswersTheFixtureBenchRightFirst(t *testing.T) {
	idx, _, b := fixtureAnswerers(t)
	rep := b.Score(idx)
	if rep.Right != rep.Total || rep.Total != 8 || rep.Score != 1 || len(rep.Misses) != 0 {
		t.Errorf("report = %+v; want all 8 right", rep)
	}
}

// The scripted baseline is what an agent does without an index: it greps the leaves for the name.
func TestTheGrepBaselineScoresLowerThanTheIndex(t *testing.T) {
	_, base, b := fixtureAnswerers(t)
	rep := b.Score(base)
	res, _ := Wiring(fixture, packs())
	t.Logf("index %v, baseline %v, misses %v", b.Score(IndexAnswerer(res, ImpactOf(res))), rep, rep.Misses)
	if rep.Total != 8 || rep.Right >= 5 {
		t.Errorf("baseline = %+v; the fixture is meant to show the index beating it", rep)
	}
	if len(rep.Misses) != rep.Total-rep.Right {
		t.Errorf("misses %v", rep.Misses)
	}
}

// Review focus: an index ships only at or above the pack's threshold; a lossy index does not.
func TestAnIndexShipsOnlyAtOrAboveTheThreshold(t *testing.T) {
	_, _, b := fixtureAnswerers(t)
	res, _ := Wiring(fixture, packs())
	var lossy []Edge
	for _, e := range res.Edges {
		if e.Kind != "imports" && e.Kind != "needs" { // lose two kinds of edge
			lossy = append(lossy, e)
		}
	}
	lres := *res
	lres.Edges = lossy
	rep := b.Score(IndexAnswerer(&lres, ImpactOf(&lres)))
	if rep.Right >= rep.Total {
		t.Fatalf("a lossy index scored %+v", rep)
	}
	for _, c := range []struct {
		right, total int
		threshold    float64
		want         bool
	}{{8, 8, 1, true}, {7, 8, 0.875, true}, {7, 8, 0.9, false}, {4, 5, 0.8, true}, {3, 5, 0.8, false}, {0, 0, 0.5, false}} {
		if got := (Report{Right: c.right, Total: c.total}).Ships(c.threshold); got != c.want {
			t.Errorf("%d/%d at %v: Ships() = %v", c.right, c.total, c.threshold, got)
		}
	}
	if rep.Ships(0.9) {
		t.Errorf("the lossy index %+v ships at 0.9", rep)
	}
}

func TestABenchThatIsNotExactIsRefused(t *testing.T) {
	dir := t.TempDir()
	good := "schema: ygg.routing-bench/v1\nquestions:\n  - { id: q, kind: consumers, about: api, answer: [web] }\n"
	for name, body := range map[string]string{
		"an unknown field":  strings.Replace(good, "answer:", "model: x, answer:", 1),
		"another schema":    strings.Replace(good, "bench/v1", "bench/v2", 1),
		"no question":       "schema: ygg.routing-bench/v1\nquestions: []\n",
		"an unknown kind":   strings.Replace(good, "consumers", "guess", 1),
		"a repeated id":     good + "  - { id: q, kind: consumers, about: api, answer: [] }\n",
		"no id":             strings.Replace(good, "id: q", "id: ''", 1),
		"a pin with no arg": strings.Replace(good, "kind: consumers", "kind: pin", 1),
		"no answer list":    strings.Replace(good, ", answer: [web]", "", 1),
		"more than one doc": good + "---\nschema: x\n",
	} {
		p := filepath.Join(dir, "bench.yaml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := LoadBench(p); err == nil {
			t.Errorf("%s: LoadBench() = nil, want an error", name)
		}
	}
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644)
	if _, err := LoadBench(filepath.Join(dir, "good.yaml")); err != nil {
		t.Error(err)
	}
	os.Symlink(filepath.Join(dir, "good.yaml"), filepath.Join(dir, "link.yaml"))
	if _, err := LoadBench(filepath.Join(dir, "link.yaml")); err == nil {
		t.Error("a bench behind a link is read")
	}
	big := filepath.Join(dir, "big.yaml")
	os.WriteFile(big, []byte(good+"# "+strings.Repeat("x", maxFile)+"\n"), 0o644)
	if _, err := LoadBench(big); err == nil {
		t.Error("a bench over the cap is read")
	}
}

func TestABenchBehindALinkedFolderIsRefused(t *testing.T) {
	root, real := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(real, "b.yaml"), []byte("schema: ygg.routing-bench/v1\nquestions:\n  - { id: q, kind: consumers, about: api, answer: [] }\n"), 0o644)
	os.Symlink(real, filepath.Join(root, ".ygg"))
	if _, err := LoadBenchIn(root, ".ygg/b.yaml"); err == nil {
		t.Error("a bench behind a linked folder is read")
	}
	if _, err := LoadBenchIn(root, "../b.yaml"); err == nil {
		t.Error("a bench outside the project is read")
	}
	if _, err := LoadBenchIn(real, "b.yaml"); err != nil {
		t.Error(err)
	}
}
