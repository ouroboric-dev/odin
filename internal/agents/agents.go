// Package agents builds the agent adapter a role names.
package agents

import (
	"fmt"
	"path/filepath"

	"github.com/ouroboric-dev/gleipnir/scripted"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// New returns the adapter for role. A scripted role's script path is relative to configDir.
// Phase 3 adds claude-code and opencode; until then a role that names them can't start.
func New(role schema.Role, configDir string) (gungnir.Agent, error) {
	switch role.Agent {
	case "scripted":
		p := role.Script
		if !filepath.IsAbs(p) {
			p = filepath.Join(configDir, p)
		}
		return scripted.Load(p)
	default:
		return nil, fmt.Errorf("agent %q isn't in this build of ygg yet", role.Agent)
	}
}
