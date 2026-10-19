package routing

import (
	"flag"
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

func TestWiringReadsManifestsLockfilesContractsAndImports(t *testing.T) {
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
		{"worker", "imports", "api", Extracted, "worker/cmd/worker/main.go"},
		{"web", "requires", "npm:react", Extracted, "web/package.json"},
		{"web", "pins", "npm:react@19.3.0", Extracted, "web/package-lock.json"},
		{"api", "holds", "contract:api/internal/api.openapi.yaml", Extracted, "api/internal/api.openapi.yaml"},
		{"contract:web/contracts/api.openapi.yaml", "copies", "contract:api/internal/api.openapi.yaml", Extracted, "web/contracts/api.openapi.yaml"},
		{"contract:worker/internal/api.openapi.yaml", "copies", "contract:api/internal/api.openapi.yaml", Inferred, "worker/internal/api.openapi.yaml"},
		{"worker", "mentions", "web", Inferred, "worker/README.md"},
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
	if x == 0 || i == 0 || x+i != len(res.Edges) {
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

// Review focus: leaf files are written by an agent. A link must not lead the generator out of the leaf,
// and a name must not be able to break the table or the markers.
func TestWiringIgnoresSymlinksHugeFilesAndNamesThatBreakTheTable(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, fixture, dir)
	outside := filepath.Join(t.TempDir(), "secret.openapi.yaml")
	os.WriteFile(outside, []byte("openapi: 3.0.3\ninfo: {title: secret}\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "api/internal/leak.openapi.yaml")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "web/package.json"), []byte(`{"dependencies":{"a|b":"1.0.0","x\n`+EndMarker+`":"1.0.0","ok":"1.0.0"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "worker/big.go"), []byte("package big\n"+strings.Repeat("// pad\n", 200000)), 0o644)
	// Over 1 MiB, and it would give the edge worker -requires-> api if it were read.
	os.WriteFile(filepath.Join(dir, "worker/go.mod"), []byte("module example.test/worker\n\nrequire example.test/api v0.0.0\n"+strings.Repeat("// pad\n", 200000)), 0o644)
	res, err := Wiring(dir, packs())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Edges {
		if strings.Contains(e.To+e.From+e.Source, "leak") || strings.ContainsAny(e.To, "|\n") {
			t.Errorf("edge %+v came from a link or a bad name", e)
		}
	}
	if find(res.Edges, "web", "requires", "npm:ok") == nil {
		t.Error("a good dependency was dropped with the bad ones")
	}
	if find(res.Edges, "worker", "requires", "api") != nil {
		t.Error("a go.mod over the size limit gave an edge")
	}
	over := false
	for _, k := range res.Skipped {
		if strings.HasPrefix(k, "worker/go.mod: over ") {
			over = true
		}
	}
	if !over {
		t.Errorf("skipped = %v; want worker/go.mod reported as over the limit", res.Skipped)
	}
	if len(res.Skipped) < 3 {
		t.Errorf("skipped = %v; want the link, the two names and the big file reported", res.Skipped)
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

// The block of the fixture is pinned byte by byte. A change of the generator that moves a row shows here.
// Run `go test ./internal/routing -run Golden -update` to write it again, and read the diff.
func TestTheFixtureBlockMatchesItsGoldenFile(t *testing.T) {
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	golden := "../../testdata/routing/wiring.golden.md"
	got := Block(res.Edges)
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil || string(want) != string(got) {
		t.Errorf("the block differs from %s (%v):\n%s", golden, err, got)
	}
}

var update = flag.Bool("update", false, "write the golden files again")

// A leaf's realm is its own pin and nothing else: the top of the umbrella pins a graph, which is no realm.
func TestTheWiringIndexTakesALeafsRealmFromItsOwnPinOnly(t *testing.T) {
	u, err := schema.LoadUmbrella(fixture)
	if err != nil || u.Graph != "software" || u.GraphVersion != "0.1.0" {
		t.Fatalf("the fixture's umbrella = %+v, %v; want it pinned to graph software 0.1.0", u, err)
	}
	if c := u.Children["web"]; c.Realm != "midgard-react" || c.RealmVersion != "0.1.0" {
		t.Fatalf("web pins %q %q; the fixture pins it to midgard-react 0.1.0", c.Realm, c.RealmVersion)
	}
	res, err := Wiring(fixture, packs())
	if err != nil {
		t.Fatal(err)
	}
	if find(res.Edges, "web", "uses-realm", "realm:midgard-react@0.1.0") == nil {
		t.Error("web has no uses-realm edge from its own pin")
	}
	// The golden files hold the same bytes as before the graph pin: TestTheFixtureBlockMatchesItsGoldenFile
	// and TestTheImpactIndexIsTheSameBytesEveryTimeAndRoundTrips.
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.CopyFS(dir, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	v, err := schema.LoadUmbrella(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := v.Children["web"]
	c.Realm, c.RealmVersion = "", ""
	v.Children["web"] = c
	if err := v.Save(dir); err != nil {
		t.Fatal(err)
	}
	res, err = Wiring(dir, packs())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Edges {
		if e.From == "web" && (e.Kind == "uses-realm" || e.Kind == "needs") {
			t.Errorf("web pins no realm, and has the edge %+v", e)
		}
	}
}
