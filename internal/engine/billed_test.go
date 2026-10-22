package engine

import (
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func TestOnlyASessionOfAPaidAdapterIsBilled(t *testing.T) {
	u := gungnir.Usage{InTok: 100, CostUSD: 0.30, CostEstimated: true}
	for adapter, want := range map[string]float64{"opencode": 0.30, "claude-code": 0, "scripted": 0, "a-new-adapter": 0.30} {
		if got := billed(adapter, u); got.BilledUSD != want || got.CostUSD != 0.30 || got.InTok != 100 {
			t.Errorf("billed(%q) = %+v; want BilledUSD %v and the rest as it was", adapter, got, want)
		}
	}
}
