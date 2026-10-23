// Package services says which services a leaf's sandbox starts: the runtime realm's provider of each need
// of the leaf's realm, and the services an older pack still names itself (spec §6.1, §9.4). It reads no
// run's state, so a run and ygg deps, which has none, give a leaf the same services.
package services

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
)

// LeavesFile is where the spec stage names the product's leaves and the project's runtime realm.
const LeavesFile = "docs/leaves.yaml"

// Loader loads the realms For needs: the realm of a leaf, and the project's runtime realm, which is nil
// when the project pins none.
type Loader interface {
	Leaf(repo string) (*realm.Realm, error)
	Runtime() (*realm.Realm, error)
}

// For returns the services of the sandbox that holds the leaves at repos, sorted by name: for the realm of
// each leaf and of each leaf it consumes (in u, which may be nil), the services its pack names, and the
// runtime realm's provider of each of its needs. A service keeps the name it has in its realm's pack, and
// is started once however many realms want it. Each is a copy, so a caller may change it. A need with no
// provider is an error, and so is a leaf whose realm l can't load.
func For(u *schema.Umbrella, repos []string, l Loader) ([]gungnir.Service, error) {
	leaves := slices.Clone(repos)
	if u != nil {
		for _, p := range repos {
			leaves = append(leaves, u.Children[p].Consumes...)
		}
	}
	var realms []*realm.Realm
	for _, p := range leaves {
		r, err := l.Leaf(p)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(realms, func(x *realm.Realm) bool { return x.Name == r.Name }) {
			realms = append(realms, r)
		}
	}
	set := serviceSet{where: strings.Join(repos, " and ")}
	for _, r := range realms { // every realm's own services first, so a conflict of names comes before any need
		for _, v := range r.Pack.ServiceList() {
			if err := set.add(r.Name, v); err != nil {
				return nil, err
			}
		}
	}
	var runtime *realm.Realm
	loaded := false
	var unmet error // the first, which a conflict found later outranks
	for _, r := range realms {
		for _, need := range r.Pack.Needs {
			if !loaded {
				var err error
				if runtime, err = l.Runtime(); err != nil {
					return nil, err
				}
				loaded = true
			}
			v, err := Provider(runtime, r, need)
			if _, ok := err.(*Unmet); ok {
				if unmet == nil {
					unmet = err
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			if err := set.add(runtime.Name, v); err != nil {
				return nil, err
			}
		}
	}
	if unmet != nil {
		return nil, unmet
	}
	slices.SortFunc(set.list, func(a, b gungnir.Service) int { return strings.Compare(a.Name, b.Name) })
	return set.list, nil
}

// Unmet is the error of a need with no provider.
type Unmet struct{ msg string }

func (e *Unmet) Error() string { return e.msg }

// Provider returns the service of the runtime realm that meets the need of the realm r; runtime is nil when
// the project pins none. Its error, an *Unmet, is the one message for an unmet need: it names the need, the
// realm and where the runtime realm is pinned.
func Provider(runtime, r *realm.Realm, need string) (gungnir.Service, error) {
	what := need
	for _, n := range gungnir.Needs {
		if n.Name == need {
			what = fmt.Sprintf("%s (%s)", need, n.Description)
		}
	}
	if runtime == nil {
		return gungnir.Service{}, &Unmet{fmt.Sprintf("the realm %s needs %s, and the project pins no runtime realm to provide it: name one under runtime in %s",
			r.Name, what, LeavesFile)}
	}
	svc, ok := runtime.Pack.Provider(need)
	if !ok {
		return gungnir.Service{}, &Unmet{fmt.Sprintf("the realm %s needs %s, and the runtime realm %s %s does not provide it: name one that does under runtime in %s",
			r.Name, what, runtime.Name, runtime.Version, LeavesFile)}
	}
	return svc, nil
}

// serviceSet collects the services of several realms for one sandbox, the sandbox of where. Two realms may
// offer the same service only if they offer it exactly alike.
type serviceSet struct {
	where  string
	list   []gungnir.Service
	owners map[string]string // service name: the realm that offers it
}

// add adds the service v, which the realm from offers, as a copy: a pack's service shares its maps and slices
// with the pack.
func (ss *serviceSet) add(from string, v gungnir.Service) error {
	if ss.owners == nil {
		ss.owners = map[string]string{}
	}
	i := slices.IndexFunc(ss.list, func(x gungnir.Service) bool { return x.Name == v.Name })
	switch {
	case i < 0:
		v.Env, v.Ports, v.Command = maps.Clone(v.Env), slices.Clone(v.Ports), slices.Clone(v.Command)
		ss.list = append(ss.list, v)
		ss.owners[v.Name] = from
	case !same(ss.list[i], v):
		return fmt.Errorf("the sandbox of %s would start two different services named %s: the realm %s offers one and the realm %s another",
			ss.where, v.Name, ss.owners[v.Name], from)
	}
	return nil
}

func same(a, b gungnir.Service) bool {
	return a.Image == b.Image && maps.Equal(a.Env, b.Env) && slices.Equal(a.Ports, b.Ports) && slices.Equal(a.Command, b.Command)
}
