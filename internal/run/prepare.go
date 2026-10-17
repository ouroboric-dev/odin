package run

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/agents"
	"github.com/ouroboric-dev/odin/internal/realm"
)

// SandboxSetup is what Prepare works out for a run whose leaves run in sandboxes (spec §6): where
// the engine is and how it sees the host's files, and the hosts the firewall opens. It is nil when
// the config has no sandbox section: only scripted roles then, and leaf commands run on the host.
type SandboxSetup struct {
	Engine string
	Config sandbox.Config
	Egress []string // the lead realm's registries and the model APIs of the roles the graph uses
	Hosts  []string // the model APIs of the roles the graph uses: every leaf's sandbox opens them
}

// prepareSandbox sets up the sandbox of a run, and refuses a graph that would run outside it.
func (s *Setup) prepareSandbox() error {
	if s.Config.Sandbox == nil {
		return nil
	}
	if s.Pack.Sandbox == nil {
		return fmt.Errorf("%s has a sandbox section, but the realm %s names no sandbox stack in its pack.yaml", s.ConfigPath, s.Pack.Name)
	}
	for _, n := range s.Graph.All() {
		// Acceptance checks run in the project umbrella, over several leaves, so there is no one leaf
		// to mount. Until a snapshot sandbox exists (Phase 5), they can't run with a sandbox on.
		if len(n.Acceptance) > 0 {
			return fmt.Errorf("node %s runs acceptance checks, which run in the project umbrella and so outside any sandbox; a realm can use them once a sandbox for them exists (Phase 3, decision 11)", n.ID)
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return fmt.Errorf("ygg must not run as root: the sandbox's user is the operator's own")
	}
	cfg := sandbox.Config{Images: sandbox.ImagesFor(s.Pack.Sandbox.Stack), UID: uid, GID: gid}
	for _, m := range s.Config.Sandbox.Mounts {
		cfg.Mounts = append(cfg.Mounts, sandbox.PathMap{Host: m.Host, Engine: m.Engine})
	}
	var hosts []string
	for _, n := range s.Graph.All() {
		for _, role := range nodeRoles(n) {
			hosts = append(hosts, agents.Hosts(s.Config.Roles[role].Agent)...)
		}
	}
	hosts = sortedUnique(hosts)
	s.Sandbox = &SandboxSetup{Engine: s.Config.Sandbox.Engine, Config: cfg, Hosts: hosts, Egress: sortedUnique(slices.Concat(s.Pack.Sandbox.Egress, hosts))}
	// A leaf of another realm starts on that realm's stack, so every leaf of the run must have one
	// before anything starts, not when its sandbox comes up.
	for _, p := range s.Leaves() {
		if _, _, _, err := s.SandboxFor(p); err != nil {
			return err
		}
	}
	return nil
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

// SandboxFor says what the sandbox of the leaf at repo starts: the stack layer and the registries of the
// realm that provides the leaf, and the services of that realm and of each leaf it consumes (spec §6.1,
// §9.2, change realm-leaf-naming). The model APIs of the run's roles are open in every sandbox.
func (s *Setup) SandboxFor(repo string) (stack string, egress []string, services []gungnir.Service, err error) {
	r := s.RealmOf(repo)
	if r.Pack.Sandbox == nil {
		return "", nil, nil, fmt.Errorf("the leaf %s is provided by the realm %s, which names no sandbox stack in its pack.yaml, and %s has a sandbox section", repo, r.Name, s.ConfigPath)
	}
	owners := map[string]string{} // service name: the realm that offers it
	add := func(from *realm.Realm) error {
		for _, v := range from.Pack.ServiceList() {
			i := slices.IndexFunc(services, func(x gungnir.Service) bool { return x.Name == v.Name })
			switch {
			case i < 0:
				services = append(services, v)
				owners[v.Name] = from.Name
			case !sameService(services[i], v):
				return fmt.Errorf("the sandbox of %s would start two different services named %s: the realm %s offers one and the realm %s another", repo, v.Name, owners[v.Name], from.Name)
			}
		}
		return nil
	}
	if err := add(r); err != nil {
		return "", nil, nil, err
	}
	if u := s.Project.Umbrella; u != nil {
		for _, c := range u.Children[repo].Consumes {
			if err := add(s.RealmOf(c)); err != nil {
				return "", nil, nil, err
			}
		}
	}
	slices.SortFunc(services, func(a, b gungnir.Service) int { return strings.Compare(a.Name, b.Name) })
	return r.Pack.Sandbox.Stack, sortedUnique(slices.Concat(r.Pack.Sandbox.Egress, s.Sandbox.Hosts)), services, nil
}

func sameService(a, b gungnir.Service) bool {
	return a.Image == b.Image && maps.Equal(a.Env, b.Env) && slices.Equal(a.Ports, b.Ports)
}

// prepareSkills opens the libraries and records the skills the graph names (spec §5.1). A node that
// names a skill nobody has vendored refuses the run before it starts.
func (s *Setup) prepareSkills(override *skills.Libraries) error {
	if override != nil {
		s.Libs = *override
	} else {
		pinned, err := havamal.Embedded()
		if err != nil {
			return err
		}
		s.Libs.Pinned = pinned
		if dir := s.Config.Skills.PrivateDir; dir != "" {
			if s.Libs.Private, err = havamal.OpenPrivate(dir); err != nil {
				return err
			}
		}
	}
	var names []string
	for _, n := range s.Graph.All() {
		names = append(names, n.Skills...)
		for _, rv := range n.Reviewers {
			names = append(names, rv.Skills...)
		}
	}
	rec, err := s.Libs.Record(names)
	if err != nil {
		return fmt.Errorf("the graph names a skill the library lacks: %w", err)
	}
	s.Skills = rec
	return nil
}

// nodeRoles returns the roles a node's sessions run as: an agent node's, or a loop's implementer and
// its reviewers.
func nodeRoles(n schema.Node) []string {
	switch n.Kind {
	case schema.KindAgent:
		return []string{n.Role}
	case schema.KindLoop:
		out := []string{n.Role}
		for _, rv := range n.Reviewers {
			out = append(out, rv.Role)
		}
		return out
	}
	return nil
}
