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
	if s.GraphPack.Pack.Sandbox == nil {
		return fmt.Errorf("%s has a sandbox section, but the realm %s names no sandbox stack in its pack.yaml", s.ConfigPath, s.GraphPack.Pack.Name)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return fmt.Errorf("ygg must not run as root: the sandbox's user is the operator's own")
	}
	cfg := sandbox.Config{Images: sandbox.ImagesFor(s.GraphPack.Stack), UID: uid, GID: gid}
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
	s.Sandbox = &SandboxSetup{Engine: s.Config.Sandbox.Engine, Config: cfg, Hosts: hosts, Egress: sortedUnique(slices.Concat(s.GraphPack.Pack.Sandbox.Egress, hosts))}
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
	r, err := s.RealmOf(repo)
	if err != nil {
		return "", nil, nil, err
	}
	if r.Pack.Sandbox == nil {
		return "", nil, nil, fmt.Errorf("the leaf %s is provided by the realm %s, which names no sandbox stack in its pack.yaml, and %s has a sandbox section", repo, r.Name, s.ConfigPath)
	}
	var set serviceSet
	if err := set.add(repo, r); err != nil {
		return "", nil, nil, err
	}
	if u := s.Project.Umbrella; u != nil {
		for _, c := range u.Children[repo].Consumes {
			cr, err := s.RealmOf(c)
			if err != nil {
				return "", nil, nil, err
			}
			if err := set.add(repo, cr); err != nil {
				return "", nil, nil, err
			}
		}
	}
	services = set.list
	slices.SortFunc(services, func(a, b gungnir.Service) int { return strings.Compare(a.Name, b.Name) })
	return r.Stack, sortedUnique(slices.Concat(r.Pack.Sandbox.Egress, s.Sandbox.Hosts)), services, nil
}

// serviceSet collects the services of several realms for one sandbox. Two realms may offer the same service
// only if they offer it exactly alike.
type serviceSet struct {
	list   []gungnir.Service
	owners map[string]string // service name: the realm that offers it
}

// add adds the services of realm from to the sandbox of where (a leaf, or the snapshot).
func (ss *serviceSet) add(where string, from *realm.Realm) error {
	if ss.owners == nil {
		ss.owners = map[string]string{}
	}
	for _, v := range from.Pack.ServiceList() {
		i := slices.IndexFunc(ss.list, func(x gungnir.Service) bool { return x.Name == v.Name })
		switch {
		case i < 0:
			ss.list = append(ss.list, v)
			ss.owners[v.Name] = from.Name
		case !sameService(ss.list[i], v):
			return fmt.Errorf("the sandbox of %s would start two different services named %s: the realm %s offers one and the realm %s another", where, v.Name, ss.owners[v.Name], from.Name)
		}
	}
	return nil
}

// projectLeaves are the leaves the sandbox of a snapshot holds: those the run works in and those the
// project's umbrella records, sorted.
func (s *Setup) projectLeaves() []string {
	paths := s.Leaves()
	if u := s.Project.Umbrella; u != nil {
		for p, c := range u.Children {
			if c.Kind == schema.ChildLeaf {
				paths = append(paths, p)
			}
		}
	}
	return sortedUnique(paths)
}

// leafRealms are the distinct realms of the project's leaves, sorted by name: the realm each leaf of the
// project provides, and the graph pack when Start creates a leaf from it. extra are realms of leaves that
// init is about to create.
func (s *Setup) leafRealms(extra []*realm.Realm) []*realm.Realm {
	var out []*realm.Realm
	add := func(r *realm.Realm) {
		if r != nil && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	for _, p := range s.projectLeaves() {
		if r, err := s.RealmOf(p); err == nil { // Prepare has refused a leaf with no realm
			add(r)
		}
	}
	if len(s.Create) > 0 {
		add(s.GraphPack)
	}
	for _, r := range extra {
		add(r)
	}
	slices.SortFunc(out, func(a, b *realm.Realm) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// AcceptanceRealms are the realms of the project's leaves, and of the leaves Start creates, that define the
// acceptance check, sorted by name: a pointer move runs the check of each, in this order (change voluspa).
func (s *Setup) AcceptanceRealms(check string) []*realm.Realm { return s.acceptanceRealms(check, nil) }

func (s *Setup) acceptanceRealms(check string, extra []*realm.Realm) []*realm.Realm {
	return slices.DeleteFunc(s.leafRealms(extra), func(r *realm.Realm) bool { return len(r.Pack.Checks[check]) == 0 })
}

// CheckAcceptance refuses, before any leaf exists or any sandbox starts, a graph whose nodes ask for an
// acceptance check that no leaf's realm defines, and, with a sandbox, a realm that defines one and names no
// acceptance stack to run it on. extra are the realms of the leaves init is about to create. A project with
// no leaf yet is not refused here: init calls this again once the leaves file names the leaves.
func (s *Setup) CheckAcceptance(extra []*realm.Realm) error {
	if len(s.projectLeaves()) == 0 && len(s.Create) == 0 && len(extra) == 0 {
		return nil
	}
	for _, n := range s.Graph.All() {
		for _, c := range n.Acceptance {
			realms := s.acceptanceRealms(c, extra)
			if len(realms) == 0 {
				return fmt.Errorf("node %s runs the acceptance check %s, and no realm of this project has an acceptance check %s: a move would check less than it does today", n.ID, c, c)
			}
			if s.Config.Sandbox == nil {
				continue
			}
			for _, r := range realms {
				if r.Acceptance == "" {
					return fmt.Errorf("node %s runs acceptance checks, which need a sandbox that holds a snapshot of the project; the realm %s defines the check %s and names no acceptance stack (sandbox.acceptance in its pack.yaml)", n.ID, r.Name, c)
				}
			}
		}
	}
	return nil
}

// SnapshotSandbox says what the sandbox that holds a snapshot of the project starts, for the acceptance
// checks of realm rl (change voluspa): rl's acceptance stack, the services of the realm of every leaf of
// the project, since the checks run the whole product, and the registries of those realms. Never wider
// than the leaves' own realms give.
func (s *Setup) SnapshotSandbox(rl *realm.Realm) (stack string, egress []string, services []gungnir.Service, err error) {
	if rl.Acceptance == "" {
		return "", nil, nil, fmt.Errorf("the realm %s names no acceptance stack, so there is no sandbox for a snapshot of the project (sandbox.acceptance in its pack.yaml)", rl.Name)
	}
	var set serviceSet
	for _, r := range s.leafRealms(nil) {
		if err := set.add("the snapshot", r); err != nil {
			return "", nil, nil, err
		}
		if r.Pack.Sandbox != nil {
			egress = append(egress, r.Pack.Sandbox.Egress...)
		}
	}
	slices.SortFunc(set.list, func(a, b gungnir.Service) int { return strings.Compare(a.Name, b.Name) })
	return rl.Acceptance, sortedUnique(egress), set.list, nil
}

func sameService(a, b gungnir.Service) bool {
	return a.Image == b.Image && maps.Equal(a.Env, b.Env) && slices.Equal(a.Ports, b.Ports) && slices.Equal(a.Command, b.Command)
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
