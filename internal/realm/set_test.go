package realm

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

// store writes realm leaves at the given versions and returns the loader of a run.
func store(t *testing.T, versions map[string]string) func(string) (*Realm, error) {
	t.Helper()
	root := t.TempDir()
	for name, v := range versions {
		fixture(t, filepath.Join(root, name), name, v)
	}
	s := &Store{Dir: root, Kind: "realm"}
	return func(name string) (*Realm, error) {
		dir, err := s.Find(name)
		if err != nil {
			return nil, err
		}
		return Load(dir)
	}
}

func TestEachRealmLoadsOnceAtThePinnedVersion(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.1.0", "midgard-vue": "0.3.0"})
	graph, err := load("midgard-react")
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	counting := func(name string) (*Realm, error) { loads++; return load(name) }
	set, err := Collect([]*Realm{graph}, []schema.Pin{
		{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"},
		{Realm: "helheim-go", Version: "0.1.0", Where: "repos/api"},
		{Realm: "midgard-react", Version: "0.1.0", Where: "repos/web"},
		{Realm: "midgard-react", Version: "0.1.0", Where: "repos/admin"},
		{Realm: "midgard-vue", Version: "0.3.0", Where: "repos/storefront"},
	}, counting)
	if err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	if loads != 2 {
		t.Errorf("%d realms loaded, want 2 (the first realm is already loaded, and each other realm once)", loads)
	}
	if len(set.Realms) != 3 || set.Realms["helheim-go"].Version != "0.1.0" || set.Realms["midgard-react"] != graph {
		t.Errorf("set = %+v", set)
	}
	if got := set.Names(); strings.Join(got, ",") != "helheim-go,midgard-react,midgard-vue" {
		t.Errorf("Names() = %v", got)
	}
}

func TestTwoPinsOfOneRealmThatDisagreeStopTheRun(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.1.0"})
	graph, _ := load("midgard-react")
	_, err := Collect([]*Realm{graph}, []schema.Pin{
		{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"},
		{Realm: "midgard-react", Version: "0.2.0", Where: "repos/web"},
		{Realm: "helheim-go", Version: "0.1.0", Where: "repos/api"},
		{Realm: "helheim-go", Version: "0.1.1", Where: "repos/db"},
	}, load)
	if err == nil {
		t.Fatal("Collect() = nil with two pins of one realm at different versions")
	}
	for _, want := range []string{"midgard-react is pinned at 0.1.0 by repos/app and at 0.2.0 by repos/web", "helheim-go is pinned at 0.1.0 by repos/api and at 0.1.1 by repos/db"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q:\n%v", want, err)
		}
	}
}

func TestTheRealmStoreMustHoldTheVersionThePinNames(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.2.0", "helheim-go": "0.1.0"})
	graph, _ := load("midgard-react")
	_, err := Collect([]*Realm{graph}, []schema.Pin{{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"}, {Realm: "helheim-go", Version: "0.2.0", Where: "repos/api"}}, load)
	if err == nil || !strings.Contains(err.Error(), "the realm store holds midgard-react 0.2.0, but repos/app pins 0.1.0") || !strings.Contains(err.Error(), "helheim-go 0.1.0, but repos/api pins 0.2.0") {
		t.Errorf("Collect() = %v", err)
	}
	_, err = Collect([]*Realm{graph}, []schema.Pin{{Realm: "asgard", Version: "0.1.0", Where: "repos/x"}}, load)
	if err == nil || !strings.Contains(err.Error(), "repos/x") || !strings.Contains(err.Error(), "asgard") {
		t.Errorf("a realm the store lacks: %v", err)
	}
}

// A leaf pins its realm, and the graph pack is a realm of the run only when it holds a recipe: Collect
// loads the realms the leaves pin, takes the ones it is given, and invents none.
func TestCollectLoadsOnlyTheRealmsTheLeavesPin(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.1.0", "midgard-vue": "0.1.0", "helheim-go": "0.1.0"})
	vue, _ := load("midgard-vue")
	loaded := []string{}
	counting := func(name string) (*Realm, error) { loaded = append(loaded, name); return load(name) }
	set, err := Collect(nil, []schema.Pin{{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"}}, counting)
	if err != nil || strings.Join(set.Names(), ",") != "midgard-react" || strings.Join(loaded, ",") != "midgard-react" {
		t.Errorf("one pin, nothing in hand: %v, realms %v, loaded %v", err, set, loaded)
	}
	set, err = Collect([]*Realm{vue}, []schema.Pin{{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"}}, load)
	if err != nil || strings.Join(set.Names(), ",") != "midgard-react,midgard-vue" {
		t.Errorf("a realm in hand and one pin: %v, %v", err, set)
	}
	before := len(loaded)
	set, err = Collect([]*Realm{vue}, nil, counting)
	if err != nil || strings.Join(set.Names(), ",") != "midgard-vue" || len(loaded) != before {
		t.Errorf("a realm in hand and no pins: %v, %v; loaded %v", err, set, loaded)
	}
	set, err = Collect(nil, nil, counting)
	if err != nil || len(set.Realms) != 0 || len(loaded) != before {
		t.Errorf("no pins and nothing in hand: %v, %+v; loaded %v", err, set, loaded)
	}
}
