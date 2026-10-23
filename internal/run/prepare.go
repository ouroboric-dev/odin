package run

import (
	"errors"
	"fmt"
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
	"github.com/ouroboric-dev/odin/internal/services"
)

// SandboxSetup is what Prepare works out for a run whose leaves run in sandboxes (spec §6): where
// the engine is and how it sees the host's files, and the hosts the firewall opens. It is nil when
// the config has no sandbox section: only scripted roles then, and leaf commands run on the host.
// Config.Images are the factory layer's: a sandbox that names no stack, such as a scratch session's,
// starts on them.
type SandboxSetup struct {
	Engine string
	Config sandbox.Config
	Hosts  []string // the model APIs of the roles the graph uses: every sandbox of the run opens them, and a scratch session opens nothing else
}

// prepareSandbox sets up the sandbox of a run, and refuses a graph that would run outside it. With initAhead, a
// need with no provider is init's to refuse: the runtime realm it pins may provide it, and it refuses before
// any leaf sandbox starts.
func (s *Setup) prepareSandbox(initAhead bool) error {
	if s.Config.Sandbox == nil {
		return nil
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return fmt.Errorf("ygg must not run as root: the sandbox's user is the operator's own")
	}
	cfg := sandbox.Config{Images: sandbox.FactoryImages(), UID: uid, GID: gid}
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
	s.Sandbox = &SandboxSetup{Engine: s.Config.Sandbox.Engine, Config: cfg, Hosts: hosts}
	// A leaf of another realm starts on that realm's stack, so every leaf of the run must have one
	// before anything starts, not when its sandbox comes up.
	for _, p := range s.Leaves() {
		var unmet *services.Unmet // with init ahead, an unmet need is init's to refuse: it may pin the provider (T07)
		if _, _, _, err := s.SandboxFor(p); err != nil && !(initAhead && errors.As(err, &unmet)) {
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
// realm that provides the leaf, and the services services.For gives the leaf: the runtime realm's provider of
// each need of that realm and of the realm of each leaf it consumes (spec §6.1, §9.2, §9.4, change
// realm-leaf-naming). The model APIs of the run's roles are open in every sandbox.
func (s *Setup) SandboxFor(repo string) (stack string, egress []string, svcs []gungnir.Service, err error) {
	r, err := s.RealmOf(repo)
	if err != nil {
		return "", nil, nil, err
	}
	if r.Pack.Sandbox == nil || r.Stack == "" {
		return "", nil, nil, fmt.Errorf("the leaf %s is provided by the realm %s, which names no sandbox stack in its pack.yaml, and %s has a sandbox section", repo, r.Name, s.ConfigPath)
	}
	if svcs, err = services.For(s.Project.Umbrella, []string{repo}, setupLoader{s}); err != nil {
		return "", nil, nil, err
	}
	return r.Stack, sortedUnique(slices.Concat(r.Pack.Sandbox.Egress, s.Sandbox.Hosts)), svcs, nil
}

// setupLoader is the services.Loader of a run: the realms it loaded.
type setupLoader struct{ s *Setup }

func (l setupLoader) Leaf(repo string) (*realm.Realm, error) { return l.s.RealmOf(repo) }
func (l setupLoader) Runtime() (*realm.Realm, error)         { return l.s.Runtime, nil }

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
// A leaf whose realm does not resolve is an error, never a leaf that adds nothing. Realms are told apart
// by name: init's planned realms are not the pointers the run loaded.
func (s *Setup) leafRealms(extra []*realm.Realm) ([]*realm.Realm, error) {
	var out []*realm.Realm
	add := func(r *realm.Realm) {
		if r != nil && !slices.ContainsFunc(out, func(x *realm.Realm) bool { return x.Name == r.Name }) {
			out = append(out, r)
		}
	}
	for _, p := range s.projectLeaves() {
		r, err := s.RealmOf(p)
		if err != nil {
			return nil, err
		}
		add(r)
	}
	if len(s.Create) > 0 {
		add(s.GraphPack)
	}
	for _, r := range extra {
		add(r)
	}
	slices.SortFunc(out, func(a, b *realm.Realm) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// AcceptanceRealms are the realms of the project's leaves, and of the leaves Start creates, that define the
// acceptance check, sorted by name: a pointer move runs the check of each, in this order (change voluspa).
// A leaf whose realm does not resolve is an error.
func (s *Setup) AcceptanceRealms(check string) ([]*realm.Realm, error) {
	return s.acceptanceRealms(check, nil)
}

func (s *Setup) acceptanceRealms(check string, extra []*realm.Realm) ([]*realm.Realm, error) {
	all, err := s.leafRealms(extra)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(r *realm.Realm) bool { return len(r.Pack.Checks[check]) == 0 }), nil
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
			realms, err := s.acceptanceRealms(c, extra)
			if err != nil {
				return err
			}
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
// checks of realm rl (change voluspa): rl's acceptance stack, the services services.For gives every leaf of
// the project, since the checks run the whole product, and the registries of those leaves' realms. Never
// wider than the leaves' realms and the runtime realm give.
func (s *Setup) SnapshotSandbox(rl *realm.Realm) (stack string, egress []string, svcs []gungnir.Service, err error) {
	if rl.Acceptance == "" {
		return "", nil, nil, fmt.Errorf("the realm %s names no acceptance stack, so there is no sandbox for a snapshot of the project (sandbox.acceptance in its pack.yaml)", rl.Name)
	}
	leaves, err := s.leafRealms(nil)
	if err != nil {
		return "", nil, nil, err
	}
	for _, r := range leaves {
		if r.Pack.Sandbox != nil {
			egress = append(egress, r.Pack.Sandbox.Egress...)
		}
	}
	if svcs, err = services.For(s.Project.Umbrella, sortedUnique(slices.Concat(s.projectLeaves(), s.Create)), setupLoader{s}); err != nil {
		return "", nil, nil, fmt.Errorf("the snapshot of the project: %w", err)
	}
	return rl.Acceptance, sortedUnique(egress), svcs, nil
}

// CheckNeeds refuses, before any leaf is created or any leaf sandbox starts, a need of a realm of the project's
// leaves that the project's runtime realm does not provide, or any need in a project that pins no runtime realm
// (spec §6.1, §9.4). extra are the realms of the leaves init is about to create. Every unmet need is listed, on one line with the first problem leading, since a stage failure shows the first line only.
func (s *Setup) CheckNeeds(extra []*realm.Realm) error { return s.CheckNeedsWith(s.Runtime, extra) }

// CheckNeedsWith is CheckNeeds with runtime as the project's runtime realm: the one a leaves file pins, before
// init records it. runtime is nil when there is none.
func (s *Setup) CheckNeedsWith(runtime *realm.Realm, extra []*realm.Realm) error {
	realms, err := s.leafRealms(extra)
	if err != nil {
		return err
	}
	var problems []string
	for _, r := range realms {
		for _, need := range r.Pack.Needs {
			if _, err := services.Provider(runtime, r, need); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s (no leaf is created and no leaf sandbox starts)", strings.Join(problems, "; "))
	}
	return nil
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
