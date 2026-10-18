package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

const fixture = "../../testdata/routing/project"

func packs() map[string]*schema.Pack {
	return map[string]*schema.Pack{
		"helheim-go":    {Name: "helheim-go", Services: map[string]schema.PackService{"postgres": {}, "seaweedfs": {}}},
		"midgard-react": {Name: "midgard-react"},
	}
}

func find(es []Edge, from, kind, to string) *Edge {
	for i, e := range es {
		if e.From == from && e.Kind == kind && e.To == to {
			return &es[i]
		}
	}
	return nil
}

func TestWiringReadsTheUmbrellaThePacksManifestsAndLockfiles(t *testing.T) {
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct{ from, kind, to, tag, source string }{
		{"web", "consumes", "api", Extracted, "umbrella.yaml"},
		{"web", "uses-realm", "realm:midgard-react@0.1.0", Extracted, "umbrella.yaml"},
		{"api", "uses-realm", "realm:helheim-go@0.1.0", Extracted, "umbrella.yaml"},
		{"api", "needs", "service:postgres", Extracted, "realm:helheim-go/pack.yaml"},
		{"api", "requires", "go:github.com/go-chi/chi/v5", Extracted, "api/go.mod"},
		{"api", "pins", "go:github.com/go-chi/chi/v5@v5.2.3", Extracted, "api/go.sum"},
		{"worker", "requires", "api", Extracted, "worker/go.mod"},
		{"web", "requires", "npm:react", Extracted, "web/package.json"},
		{"web", "pins", "npm:react@19.3.0", Extracted, "web/package-lock.json"},
	} {
		e := find(res.Edges, w.from, w.kind, w.to)
		if e == nil {
			t.Errorf("no edge %s -%s-> %s", w.from, w.kind, w.to)
			continue
		}
		if e.Tag != w.tag || e.Source != w.source {
			t.Errorf("edge %s -%s-> %s is %s from %s, want %s from %s", w.from, w.kind, w.to, e.Tag, e.Source, w.tag, w.source)
		}
	}
	if find(res.Edges, "api", "requires", "go:github.com/jackc/pgx/v5") != nil {
		t.Error("an indirect requirement is a direct edge")
	}
}

func TestEveryEdgeIsTaggedExtractedOrInferred(t *testing.T) {
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Edges {
		if e.Tag != Extracted && e.Tag != Inferred || e.From == "" || e.To == "" || e.Kind == "" || e.Source == "" {
			t.Errorf("bad edge %+v", e)
		}
	}
	x, i := Counts(res.Edges)
	if x == 0 || x+i != len(res.Edges) {
		t.Errorf("counts %d extracted, %d inferred of %d", x, i, len(res.Edges))
	}
}

func TestWiringIsTheSameBytesEveryTime(t *testing.T) {
	var first []byte
	for range 3 {
		res, err := Wiring(fixture, packs())
		if err != nil {
			t.Fatal(err)
		}
		b := Block(res.Edges)
		if first == nil {
			first = b
		} else if string(b) != string(first) {
			t.Fatal("two runs gave two blocks")
		}
	}
	if !strings.HasPrefix(string(first), BeginMarker+"\n") || !strings.HasSuffix(string(first), EndMarker+"\n") {
		t.Errorf("block does not sit between the markers:\n%s", first)
	}
}

// Review focus: package names come from files an agent wrote. A name must not break the table or the
// markers, and must not hide the good names beside it.
func TestWiringDropsNamesThatBreakTheTable(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, fixture, dir)
	os.WriteFile(filepath.Join(dir, "web/package.json"), []byte(`{"dependencies":{"a|b":"1.0.0","x\n`+EndMarker+`":"1.0.0","ok":"1.0.0"}}`), 0o644)
	res, err := Wiring(dir, packs())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Edges {
		if strings.ContainsAny(e.To, "|\n<") {
			t.Errorf("edge %+v can break the table", e)
		}
	}
	if find(res.Edges, "web", "requires", "npm:ok") == nil {
		t.Error("a good dependency was dropped with the bad ones")
	}
	if len(res.Skipped) != 2 {
		t.Errorf("skipped = %v; want the two bad names reported", res.Skipped)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
