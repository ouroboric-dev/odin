// Package gate decides what Odin does at an approval point (spec §7).
package gate

import "github.com/ouroboric-dev/gungnir"

// Action is what Odin does at an approval point.
type Action string

const (
	Stop  Action = "stop"  // write the gate file, log gate_pending, save state and exit
	Auto  Action = "auto"  // write the gate file, log gate_auto_passed and continue
	Block Action = "block" // unattended pointer-contract: block the task and continue with the rest
)

// Facts are what the run learned that the table depends on.
type Facts struct {
	Deviation      bool // spec: the design-conformance review found a deviation
	ContractInSpec bool // pointer-contract: the approved spec contains the contract change
}

// Decide applies the mode × approval-point table of spec §7.1, then the config's overrides.
// Overrides are checked by schema.Factory.CheckGates before a run starts or continues. An unknown
// approval point stops, so a typo never passes silently.
func Decide(mode gungnir.Mode, g gungnir.Gate, overrides map[gungnir.Gate]string, f Facts) Action {
	if !g.Known() || g.Hard() || mode == gungnir.ModeStepByStep || overrides[g] == "stop" {
		return Stop
	}
	switch g {
	case gungnir.GateDesign:
		if mode == gungnir.ModeUnattended && overrides[g] == "auto" {
			return Auto
		}
		return Stop
	case gungnir.GateSpec:
		if f.Deviation {
			return Stop
		}
		return Auto
	case gungnir.GatePointerContract:
		if f.ContractInSpec {
			return Auto
		}
		if mode == gungnir.ModeUnattended {
			return Block
		}
		return Stop
	default: // plan, feature, merge, pointer
		return Auto
	}
}
