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
	if len(table)+8 != len(gungnir.Gates()) {
		t.Errorf("the table covers %d soft gates; gungnir knows %d gates", len(table), len(gungnir.Gates()))
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
