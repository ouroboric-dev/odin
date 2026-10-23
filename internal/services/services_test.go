package services

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
)

const (
	pgImage = "docker.io/library/postgres:18.1@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	s3Image = "docker.io/chrislusf/seaweedfs:4.45@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5"
)

// loader is a stand-in Loader: the realm of each leaf, and the runtime realm.
type loader struct {
	leaves  map[string]*realm.Realm
	runtime *realm.Realm
}

func (l loader) Leaf(repo string) (*realm.Realm, error) {
	if r := l.leaves[repo]; r != nil {
		return r, nil
	}
	return nil, errors.New("no realm for " + repo)
}

func (l loader) Runtime() (*realm.Realm, error) { return l.runtime, nil }

func niflheim(provides ...string) *realm.Realm {
	all := map[string]schema.PackService{
		"postgres": {Image: pgImage, Env: map[string]string{"POSTGRES_DB": "app", "POSTGRES_HOST_AUTH_METHOD": "trust"}, Ports: []int{5432}},
		"s3":       {Image: s3Image, Ports: []int{8333}, Command: []string{"server", "-s3"}},
	}
	p := &schema.Pack{Name: "niflheim-local", Version: "0.1.0", Services: map[string]schema.PackService{}, Provides: map[string]string{}}
	for _, n := range provides {
		p.Services[n], p.Provides[n] = all[n], n
	}
	return &realm.Realm{Name: p.Name, Version: p.Version, Pack: p}
}

func buildRealm(name string, needs []string, services map[string]schema.PackService) *realm.Realm {
	return &realm.Realm{Name: name, Version: "0.1.0", Pack: &schema.Pack{Name: name, Version: "0.1.0", Needs: needs, Services: services}}
}

// gallery is an api leaf whose realm needs postgres and s3, a web leaf that consumes it, and a tools leaf whose
// realm needs nothing.
func gallery(runtime *realm.Realm) (*schema.Umbrella, loader) {
	u := &schema.Umbrella{Children: map[string]schema.Child{
		"repos/api":   {Kind: schema.ChildLeaf, Realm: "helheim-go"},
		"repos/web":   {Kind: schema.ChildLeaf, Realm: "midgard-react", Consumes: []string{"repos/api"}},
		"repos/tools": {Kind: schema.ChildLeaf, Realm: "asgard"},
	}}
	api := buildRealm("helheim-go", []string{"postgres", "s3"}, nil)
	return u, loader{runtime: runtime, leaves: map[string]*realm.Realm{
		"repos/api":   api,
		"repos/web":   buildRealm("midgard-react", []string{"s3"}, nil),
		"repos/tools": buildRealm("asgard", nil, nil),
	}}
}

func TestForGivesTheProvidersOfTheNeeds(t *testing.T) {
	runtime := niflheim("postgres", "s3")
	u, l := gallery(runtime)
	want := []gungnir.Service{
		{Name: "postgres", Image: pgImage, Env: map[string]string{"POSTGRES_DB": "app", "POSTGRES_HOST_AUTH_METHOD": "trust"}, Ports: []int{5432}},
		{Name: "s3", Image: s3Image, Ports: []int{8333}, Command: []string{"server", "-s3"}},
	}
	for name, repos := range map[string][]string{
		"the api leaf":                            {"repos/api"},
		"the web leaf, which consumes the api":    {"repos/web"},
		"the api leaf and the web leaf, together": {"repos/api", "repos/web"},
	} {
		got, err := For(u, repos, l)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: For() = %+v, %v; want %+v", name, got, err, want)
		}
	}
	// The services are copies: a change to one does not reach the runtime realm's pack.
	got, _ := For(u, []string{"repos/api"}, l)
	if len(got) != 2 {
		t.Fatalf("For() = %+v; want postgres and s3", got)
	}
	got[0].Env["POSTGRES_DB"], got[0].Ports[0], got[1].Command[0] = "changed", 1, "changed"
	if pg, s3 := runtime.Pack.Services["postgres"], runtime.Pack.Services["s3"]; pg.Env["POSTGRES_DB"] != "app" || pg.Ports[0] != 5432 || s3.Command[0] != "server" {
		t.Errorf("a change to For's services reached the pack: %+v, %+v", pg, s3)
	}
	if got, err := For(u, []string{"repos/tools"}, l); err != nil || len(got) != 0 {
		t.Errorf("a leaf whose realm needs nothing: For() = %+v, %v; want none", got, err)
	}
	// A leaf of a realm that needs nothing, in a project with no runtime realm, needs none.
	u, l = gallery(nil)
	if got, err := For(u, []string{"repos/tools"}, l); err != nil || len(got) != 0 {
		t.Errorf("no runtime realm, no need: For() = %+v, %v; want none", got, err)
	}
	for name, c := range map[string]struct {
		runtime    *realm.Realm
		want, also string
	}{
		"no runtime realm":                  {nil, "the realm helheim-go needs postgres (a Postgres-compatible database)", "the project pins no runtime realm to provide it: name one under runtime in docs/leaves.yaml"},
		"a runtime realm that lacks the s3": {niflheim("postgres"), "the realm helheim-go needs s3 (an S3 API)", "the runtime realm niflheim-local 0.1.0 does not provide it"},
	} {
		u, l := gallery(c.runtime)
		got, err := For(u, []string{"repos/api"}, l)
		var unmet *Unmet
		if !errors.As(err, &unmet) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.also) {
			t.Errorf("%s: For() = %+v, %v; want an error with %q and %q", name, got, err, c.want, c.also)
		}
	}
}

func TestForKeepsARealmsOwnServices(t *testing.T) {
	cache := schema.PackService{Image: "docker.io/library/redis:8.0@sha256:2222222222222222222222222222222222222222222222222222222222222222", Ports: []int{6379}}
	u, l := gallery(niflheim("postgres", "s3"))
	l.leaves["repos/api"] = buildRealm("helheim-go", []string{"s3"}, map[string]schema.PackService{"cache": cache})
	got, err := For(u, []string{"repos/web"}, l)
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	if err != nil || strings.Join(names, ",") != "cache,s3" || got[0].Image != cache.Image || got[0].Ports[0] != 6379 {
		t.Errorf("an older pack's own service: For() = %+v, %v; want cache and s3", got, err)
	}
	// The same service, alike in both realms, is started once.
	l.leaves["repos/api"] = buildRealm("helheim-go", []string{"postgres", "s3"}, map[string]schema.PackService{"postgres": niflheim("postgres").Pack.Services["postgres"]})
	if got, err := For(u, []string{"repos/api"}, l); err != nil || len(got) != 2 || got[0].Name != "postgres" || got[1].Name != "s3" {
		t.Errorf("a service alike in both realms: For() = %+v, %v; want postgres once, and s3", got, err)
	}
	// A service of the realm's own that has a provider's name and differs from it is refused, with both realms.
	l.leaves["repos/api"] = buildRealm("helheim-go", []string{"postgres"}, map[string]schema.PackService{"postgres": {Image: pgImage, Ports: []int{5433}}})
	if got, err := For(u, []string{"repos/web"}, l); err == nil || !strings.Contains(err.Error(), "two different services named postgres") ||
		!strings.Contains(err.Error(), "helheim-go") || !strings.Contains(err.Error(), "niflheim-local") {
		t.Errorf("a realm's service that differs from the runtime realm's: For() = %+v, %v; want a refusal naming both realms", got, err)
	}
	// A leaf whose realm the loader can't load is an error, never a sandbox with fewer services.
	if got, err := For(u, []string{"repos/nowhere"}, l); err == nil {
		t.Errorf("a leaf with no realm: For() = %+v, nil", got)
	}
}
