package engine

import (
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// reachGate stops at every gate node. v0-T034 replaces this file with the approval-point table,
// gate files, Approve and Reject.
func reachGate(r *run.Run, n schema.Node) (bool, error) {
	r.State.Status, r.State.Pending = run.StatusStopped, n.ID
	return true, r.Save()
}
