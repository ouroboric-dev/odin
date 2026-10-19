package realm

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
)

// Set is the realms of one run: every realm a leaf of the project pins, each loaded once, and the graph
// pack when it holds a recipe.
type Set struct {
	Realms map[string]*Realm // by name
}

// Names returns the realms' names, sorted.
func (s *Set) Names() []string { return slices.Sorted(maps.Keys(s.Realms)) }

// Collect builds the set for a run. have are the realms already loaded: the graph pack when it holds a
// recipe, else none. pins are the pins of the project's leaves, and the only reason to load a realm; the
// project's graph pin is no realm pin, and run.Setup checks it. One version of a realm leaf serves the
// whole run (change realm-leaf-naming), so Collect refuses, before anything starts: two pins of one realm
// at different versions; and a realm store that holds another version of a realm than the one pinned.
// Every problem is listed. load finds a realm by name, and is called once for each pinned realm that is not in have.
func Collect(have []*Realm, pins []schema.Pin, load func(name string) (*Realm, error)) (*Set, error) {
	var problems []string
	byRealm := map[string][]schema.Pin{}
	for _, p := range pins {
		byRealm[p.Realm] = append(byRealm[p.Realm], p)
	}
	for _, name := range slices.Sorted(maps.Keys(byRealm)) {
		ps := byRealm[name]
		for _, p := range ps[1:] {
			if p.Version != ps[0].Version {
				problems = append(problems, fmt.Sprintf("realm %s is pinned at %s by %s and at %s by %s", name, ps[0].Version, ps[0].Where, p.Version, p.Where))
			}
		}
	}
	set := &Set{Realms: map[string]*Realm{}}
	for _, r := range have {
		set.Realms[r.Name] = r
	}
	for _, name := range slices.Sorted(maps.Keys(byRealm)) {
		r := set.Realms[name]
		if r == nil {
			var err error
			if r, err = load(name); err != nil {
				for _, p := range byRealm[name] {
					problems = append(problems, fmt.Sprintf("%s pins realm %s, which can't be loaded: %v", p.Where, name, err))
				}
				continue
			}
			set.Realms[name] = r
		}
		for _, p := range byRealm[name] {
			if p.Version != r.Version {
				problems = append(problems, fmt.Sprintf("the realm store holds %s %s, but %s pins %s", name, r.Version, p.Where, p.Version))
			}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the run's realms don't agree, so nothing started:\n  %s", strings.Join(slices.Compact(problems), "\n  "))
	}
	return set, nil
}
