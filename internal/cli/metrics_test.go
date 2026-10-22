package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

func TestYggMetricsPrintsTheFiguresOfARealRunAsTextAndAsJSON(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	code, out, errOut := ygg(t, "metrics", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"run:", id, "key-points", "factory:", "time:", "usage:", "cost:", "loops:", "interventions:", "grade:        not graded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = ygg(t, "metrics", id, "--project", dir, "--json")
	var got struct {
		Run  string `json:"run"`
		Time struct {
			Elapsed int64 `json:"elapsed_ms"`
		} `json:"time"`
		Factory []string `json:"factory"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != ExitDone || err != nil || got.Run != id || len(got.Factory) == 0 {
		t.Errorf("--json gave exit %d, %v, %+v:\n%s", code, err, got, out)
	}
}

func TestYggMetricsRefusesWhatItCannotTrustOrReadAndWritesNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	path := filepath.Join(run.Dir(dir, id), "ledger.jsonl")
	good, _ := os.ReadFile(path)
	for name, args := range map[string][]string{
		"no run id":               {"metrics", "--project", dir},
		"a path as run id":        {"metrics", "../../x", "--project", dir},
		"a run that is not there": {"metrics", "20200101-000000-aaaaaa", "--project", dir},
		"a range backwards":       {"metrics", "20300101-000000-aaaaaa..20200101-000000-aaaaaa", "--project", dir},
		"a range with no run":     {"metrics", "20200101-000000-aaaaaa..20200102-000000-aaaaaa", "--project", dir},
		"compare with one":        {"metrics", "--compare", id, "--project", dir},
	} {
		if code, _, errOut := ygg(t, args...); code != ExitRefused || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q; want a refusal", name, code, errOut)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(good), `"ygg.mode":"key-points"`, `"ygg.mode":"unattended"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := ygg(t, "metrics", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "can't be trusted") {
		t.Errorf("a changed ledger: exit %d, stderr %q; want a refusal that says it can't be trusted", code, errOut)
	}
}

func TestYggMetricsRangeListsEveryRunBetweenTwoIDsAndTotalsThem(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	first, _ := walk(t, dir, config)
	second := anotherRun(t, dir, config)
	ids := []string{first, second}
	slices.Sort(ids)
	code, out, errOut := ygg(t, "metrics", ids[0]+".."+ids[1], "--project", dir)
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	for _, want := range []string{ids[0], ids[1], "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("the range lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = ygg(t, "metrics", ids[0]+".."+ids[1], "--project", dir, "--json")
	var runs []struct{ Run string }
	if err := json.Unmarshal([]byte(out), &runs); code != ExitDone || err != nil || len(runs) != 2 {
		t.Errorf("--json of a range: exit %d, %v, %d runs", code, err, len(runs))
	}
}

// anotherRun starts a second run in a project that has one, approves every stop, and returns its id.
func anotherRun(t *testing.T, dir, config string) string {
	t.Helper()
	known := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	for _, e := range entries {
		known[e.Name()] = true
	}
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	entries, _ = os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	var id string
	for _, e := range entries {
		if !known[e.Name()] {
			id = e.Name()
		}
	}
	for code == ExitStopped {
		s, err := run.LoadState(run.Dir(dir, id))
		if err != nil {
			t.Fatal(err)
		}
		code, out, errOut = ygg(t, "approve", id, s.Pending, "--project", dir)
	}
	if code != ExitDone || id == "" {
		t.Fatalf("exit %d, run %q:\n%s%s", code, id, out, errOut)
	}
	return id
}
