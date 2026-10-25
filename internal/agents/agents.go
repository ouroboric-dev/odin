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
	Privacy string            // factory.yaml's privacy: standard or strict
	Models  *schema.ModelList // the list of local models; a role that names local/<name> needs it
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
		// The schema checks Reasoning when factory.yaml loads and opencode.New checks it again, so a bad value
		// does not pass here unseen. A claude-code or scripted role ignores it, as it does price.
		cfg := opencode.Config{Privacy: o.Privacy, Reasoning: role.Reasoning}
		if p := role.Price; p != nil {
			cfg.Price = &opencode.Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
		}
		if name, local := schema.LocalModel(role.Model); local {
			if err := fillLocal(&cfg, name, role, o.Models); err != nil {
				return nil, err
			}
		}
		return opencode.New(role.Model, cfg)
	default:
		return nil, fmt.Errorf("agent %q isn't in this build of ygg", role.Agent)
	}
}

// fillLocal sets the model server and the price of a local model in cfg from the model list. The role's price
// overrides the list's.
func fillLocal(cfg *opencode.Config, name string, role schema.Role, list *schema.ModelList) error {
	if list == nil {
		return fmt.Errorf("model local/%s needs the model list, and factory.yaml names none (models: file:)", name)
	}
	m, ok := list.Models[name]
	if !ok {
		return fmt.Errorf("model local/%s isn't in the model list", name)
	}
	cfg.Local = &opencode.Local{BaseURL: fmt.Sprintf("http://%s:%d/v1", list.Server.Name, list.Server.Port), Context: m.Context, Output: m.Output}
	if role.Price == nil {
		cfg.Price = &opencode.Price{Input: m.Price.Input, Output: m.Price.Output, CacheRead: m.Price.CacheRead, CacheWrite: m.Price.CacheWrite}
	}
	return nil
}

// Hosts returns the hosts a sandbox must open for a role's adapter: the model's API. A local model has none:
// the sandbox reaches the model server over its own network.
func Hosts(agent, model string) []string {
	if _, local := schema.LocalModel(model); local {
		return nil
	}
	switch agent {
	case "claude-code":
		return claudecode.Hosts
	case "opencode":
		return opencode.Hosts
	}
	return nil
}
