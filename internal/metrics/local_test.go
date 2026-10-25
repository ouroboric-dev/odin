package metrics

import (
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func TestMetricsCountLocalSessions(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	local := session("implement", "implementer", "opencode", "local/qwen", 1000, 100, 3, 0.05, true)
	local.Provider = "local"
	f.log(3, local)
	f.log(4, session("implement-spec", "reviewer", "opencode", "openrouter/qwen/qwen3.8-27b", 900, 90, 2, 0.01, false))
	r := mustCollect(t, f.done())
	if !near(r.Usage.CostUSD, 0.06) || !near(r.Usage.BilledUSD, 0.01) || r.Usage.LocalSessions != 1 || r.Usage.Sessions != 2 {
		t.Errorf("usage = %+v; want cost 0.06, billed 0.01, 1 local session of 2", r.Usage)
	}
	var out strings.Builder
	r.WriteText(&out)
	if !strings.Contains(out.String(), "1 sessions on local models") {
		t.Errorf("the text lacks the local line:\n%s", out.String())
	}
}
