// Package agents builds the agent adapter a role names.
package agents

import (
	"fmt"
	"path/filepath"

	"github.com/ouroboric-dev/gleipnir/claudecode"
	"github.com/ouroboric-dev/gleipnir/opencode"
	"github.com/ouroboric-dev/gleipnir/scripted"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// Options are the settings every adapter shares.
type Options struct {
	Privacy string // factory.yaml's privacy: standard or strict
}

// New returns the adapter for role. A scripted role's script path is relative to configDir. An
// adapter for a real tool runs only in a sandbox, so a config that names one has a sandbox section
// (gungnir checks that), and a refusal here, such as privacy: strict on a model the adapter can't
// tighten, stops the run before it starts.
func New(role schema.Role, configDir string, o Options) (gungnir.Agent, error) {
	switch role.Agent {
	case "scripted":
		p := role.Script
		if !filepath.IsAbs(p) {
			p = filepath.Join(configDir, p)
		}
		return scripted.Load(p)
	case "claude-code":
		return &claudecode.Agent{}, nil
	case "opencode":
		cfg := opencode.Config{Privacy: o.Privacy}
		if p := role.Price; p != nil {
			cfg.Price = &opencode.Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
		}
		return opencode.New(role.Model, cfg)
	default:
		return nil, fmt.Errorf("agent %q isn't in this build of ygg", role.Agent)
	}
}

// Hosts returns the hosts a sandbox must open for a role's adapter: the model's API.
func Hosts(agent string) []string {
	switch agent {
	case "claude-code":
		return claudecode.Hosts
	case "opencode":
		return opencode.Hosts
	}
	return nil
}
