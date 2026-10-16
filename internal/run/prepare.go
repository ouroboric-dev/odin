package run

import (
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/agents"
)

// SandboxSetup is what Prepare works out for a run whose leaves run in sandboxes (spec §6): where
// the engine is and how it sees the host's files, and the hosts the firewall opens. It is nil when
// the config has no sandbox section: only scripted roles then, and leaf commands run on the host.
type SandboxSetup struct {
	Engine string
	Config sandbox.Config
	Egress []string // the model APIs of the roles the graph uses, and the realm's registries
}

// prepareSandbox sets up the sandbox of a run, and refuses a graph that would run outside it.
func (s *Setup) prepareSandbox() error {
	if s.Config.Sandbox == nil {
		return nil
	}
	if s.Pack.Sandbox == nil {
		return fmt.Errorf("%s has a sandbox section, but the realm %s names no sandbox stack in its pack.yaml", s.ConfigPath, s.Pack.Name)
	}
	for _, n := range s.Graph.Nodes {
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
	egress := slices.Clone(s.Pack.Sandbox.Egress)
	for _, n := range s.Graph.Nodes {
		if n.Kind == schema.KindAgent {
			egress = append(egress, agents.Hosts(s.Config.Roles[n.Role].Agent)...)
		}
	}
	sort.Strings(egress)
	s.Sandbox = &SandboxSetup{Engine: s.Config.Sandbox.Engine, Config: cfg, Egress: slices.Compact(egress)}
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
	for _, n := range s.Graph.Nodes {
		names = append(names, n.Skills...)
	}
	rec, err := s.Libs.Record(names)
	if err != nil {
		return fmt.Errorf("the graph names a skill the library lacks: %w", err)
	}
	s.Skills = rec
	return nil
}
