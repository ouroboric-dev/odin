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
	s := &Store{Dir: root}
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
	lead, err := load("midgard-react")
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	counting := func(name string) (*Realm, error) { loads++; return load(name) }
	set, err := Collect(lead, []schema.Pin{
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
		t.Errorf("%d realms loaded, want 2 (the lead is already loaded, and each other realm once)", loads)
	}
	if set.Lead != lead || len(set.Realms) != 3 || set.Realms["helheim-go"].Version != "0.1.0" || set.Realms["midgard-react"] != lead {
		t.Errorf("set = %+v", set)
	}
	if got := set.Names(); strings.Join(got, ",") != "helheim-go,midgard-react,midgard-vue" {
		t.Errorf("Names() = %v", got)
	}
}

func TestTwoPinsOfOneRealmThatDisagreeStopTheRun(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.1.0", "helheim-go": "0.1.0"})
	lead, _ := load("midgard-react")
	_, err := Collect(lead, []schema.Pin{
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
	lead, _ := load("midgard-react")
	_, err := Collect(lead, []schema.Pin{{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"}, {Realm: "helheim-go", Version: "0.2.0", Where: "repos/api"}}, load)
	if err == nil || !strings.Contains(err.Error(), "the realm store holds midgard-react 0.2.0, but repos/app pins 0.1.0") || !strings.Contains(err.Error(), "helheim-go 0.1.0, but repos/api pins 0.2.0") {
		t.Errorf("Collect() = %v", err)
	}
	_, err = Collect(lead, []schema.Pin{{Realm: "asgard", Version: "0.1.0", Where: "repos/x"}}, load)
	if err == nil || !strings.Contains(err.Error(), "repos/x") || !strings.Contains(err.Error(), "asgard") {
		t.Errorf("a realm the store lacks: %v", err)
	}
}

// The project's pin names a graph, not a lead realm: a leaf that pins another realm than the lead is a
// leaf like any other.
func TestALeafMayPinAnotherRealmThanTheLeadAndNoPinsLeaveTheLeadAlone(t *testing.T) {
	load := store(t, map[string]string{"midgard-react": "0.1.0", "midgard-vue": "0.1.0"})
	lead, _ := load("midgard-vue")
	if set, err := Collect(lead, []schema.Pin{{Realm: "midgard-react", Version: "0.1.0", Where: "repos/app"}}, load); err != nil || len(set.Realms) != 2 {
		t.Errorf("Collect() = %+v, %v", set, err)
	}
	set, err := Collect(lead, nil, load)
	if err != nil {
		t.Fatalf("no pins: %v", err)
	}
	if set.Lead != lead || len(set.Realms) != 1 {
		t.Errorf("a set with no pins holds the lead alone: %+v", set)
	}
}
