package gate

import (
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

// The table of spec §7.1, one row per approval point: step-by-step, key-points, unattended.
func TestModeTimesApprovalPointTable(t *testing.T) {
	type row struct{ step, key, unattended Action }
	none := Facts{}
	table := map[gungnir.Gate]row{
		"design":           {Stop, Stop, Stop},
		"spec":             {Stop, Auto, Auto},
		"plan":             {Stop, Auto, Auto},
		"feature":          {Stop, Auto, Auto},
		"merge":            {Stop, Auto, Auto},
		"pointer":          {Stop, Auto, Auto},
		"pointer-contract": {Stop, Stop, Block},
		"escalation":       {Stop, Stop, Stop},
	}
	for _, g := range gungnir.Gates() {
		want, soft := table[g]
		if !soft {
			want = row{Stop, Stop, Stop} // every hard approval point
		}
		got := row{
			Decide(gungnir.ModeStepByStep, g, nil, none),
			Decide(gungnir.ModeKeyPoints, g, nil, none),
			Decide(gungnir.ModeUnattended, g, nil, none),
		}
		if got != want {
			t.Errorf("%s: got %v, want %v", g, got, want)
		}
	}
	soft := 0
	for _, g := range gungnir.Gates() {
		if !g.Hard() {
			soft++
		}
	}
	if len(table) != soft {
		t.Errorf("the table covers %d soft gates; gungnir knows %d", len(table), soft)
	}
}

func TestDecideFactsAndOverrides(t *testing.T) {
	cases := []struct {
		name string
		mode gungnir.Mode
		gate gungnir.Gate
		over map[gungnir.Gate]string
		f    Facts
		want Action
	}{
		{"a spec deviation stops in key-points", gungnir.ModeKeyPoints, "spec", nil, Facts{Deviation: true}, Stop},
		{"a spec deviation stops in unattended", gungnir.ModeUnattended, "spec", nil, Facts{Deviation: true}, Stop},
		{"spec: stop stops in key-points", gungnir.ModeKeyPoints, "spec", map[gungnir.Gate]string{"spec": "stop"}, Facts{}, Stop},
		{"plan: stop stops in unattended", gungnir.ModeUnattended, "plan", map[gungnir.Gate]string{"plan": "stop"}, Facts{}, Stop},
		{"design: auto passes in unattended", gungnir.ModeUnattended, "design", map[gungnir.Gate]string{"design": "auto"}, Facts{}, Auto},
		{"design: auto still stops in key-points", gungnir.ModeKeyPoints, "design", map[gungnir.Gate]string{"design": "auto"}, Facts{}, Stop},
		{"an approved contract change passes in key-points", gungnir.ModeKeyPoints, "pointer-contract", nil, Facts{ContractInSpec: true}, Auto},
		{"an approved contract change stops in step-by-step", gungnir.ModeStepByStep, "pointer-contract", nil, Facts{ContractInSpec: true}, Stop},
		{"an override can't pass a hard gate", gungnir.ModeUnattended, "budget-cap", map[gungnir.Gate]string{"budget-cap": "auto"}, Facts{}, Stop},
		{"an unknown gate stops", gungnir.ModeUnattended, "ship-it", nil, Facts{}, Stop},
	}
	for _, c := range cases {
		if got := Decide(c.mode, c.gate, c.over, c.f); got != c.want {
			t.Errorf("%s: Decide() = %s, want %s", c.name, got, c.want)
		}
	}
}

// Review focus: only a contract change that an extracted edge or oasdiff shows needs a person at
// pointer-contract; a guess alone always does, in every mode but step-by-step the gate is open when no
// contract changed.
func TestPointerContractFollowsTheJudgement(t *testing.T) {
	g := gungnir.GatePointerContract
	cases := []struct {
		name string
		mode gungnir.Mode
		f    Facts
		want Action
	}{
		{"no contract change, key-points", gungnir.ModeKeyPoints, Facts{NoContractChange: true}, Auto},
		{"no contract change, unattended", gungnir.ModeUnattended, Facts{NoContractChange: true}, Auto},
		{"no contract change, step-by-step", gungnir.ModeStepByStep, Facts{NoContractChange: true}, Stop},
		{"a contract change, key-points", gungnir.ModeKeyPoints, Facts{}, Stop},
		{"a contract change, unattended", gungnir.ModeUnattended, Facts{}, Block},
		{"a contract change the spec holds", gungnir.ModeUnattended, Facts{ContractInSpec: true}, Auto},
		{"a guess alone, key-points", gungnir.ModeKeyPoints, Facts{Escalate: true}, Stop},
		{"a guess alone stops even when the spec holds the change", gungnir.ModeUnattended, Facts{Escalate: true, ContractInSpec: true}, Stop},
		{"a guess alone is not a no-change", gungnir.ModeKeyPoints, Facts{Escalate: true, NoContractChange: true}, Stop},
	}
	for _, c := range cases {
		if got := Decide(c.mode, g, nil, c.f); got != c.want {
			t.Errorf("%s: Decide() = %s, want %s", c.name, got, c.want)
		}
	}
}

// A spec that still does not cite every owner decision after Odin sent it back once stops its gate for a person in
// every mode, whatever the overrides say (typed-T19).
func TestAnUncitedOwnerDecisionStopsTheGate(t *testing.T) {
	for _, mode := range []gungnir.Mode{gungnir.ModeKeyPoints, gungnir.ModeUnattended} {
		for _, over := range []map[gungnir.Gate]string{nil, {gungnir.GateSpec: "auto"}} {
			if got := Decide(mode, gungnir.GateSpec, over, Facts{Uncited: true}); got != Stop {
				t.Errorf("%s, overrides %v: Decide() = %s, want stop", mode, over, got)
			}
		}
	}
}
