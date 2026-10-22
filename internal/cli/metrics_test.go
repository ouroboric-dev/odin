package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
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

// snapshot lists every path under dir with its size and modification time.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		got[p] = fmt.Sprintf("%d %d %v", fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameTree(t *testing.T, when string, before, after map[string]string) {
	t.Helper()
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s: %s changed from %q to %q", when, p, v, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s: %s was created", when, p)
		}
	}
}

func TestYggMetricsRefusesWhatItCannotTrustOrReadAndWritesNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	path := filepath.Join(run.Dir(dir, id), "ledger.jsonl")
	good, _ := os.ReadFile(path)
	before := snapshot(t, dir)
	if code, out, errOut := ygg(t, "metrics", id, "--project", dir); code != ExitDone || out == "" {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	sameTree(t, "a successful ygg metrics", before, snapshot(t, dir))
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
		sameTree(t, name, before, snapshot(t, dir))
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(good), `"ygg.mode":"key-points"`, `"ygg.mode":"unattended"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := snapshot(t, dir)
	if code, _, errOut := ygg(t, "metrics", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "can't be trusted") {
		t.Errorf("a changed ledger: exit %d, stderr %q; want a refusal that says it can't be trusted", code, errOut)
	}
	sameTree(t, "a changed ledger", changed, snapshot(t, dir))
}

// A range with a run whose ledger was changed in the middle is refused whole: no figures for the other runs.
func TestYggMetricsRangeRefusesWholeWhenTheMiddleRunsLedgerWasChanged(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	first, _ := walk(t, dir, config)
	second := anotherRun(t, dir, config)
	third := anotherRun(t, dir, config)
	ids := []string{first, second, third}
	slices.Sort(ids)
	path := filepath.Join(run.Dir(dir, ids[1]), "ledger.jsonl")
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(good), `"ygg.mode":"key-points"`, `"ygg.mode":"unattended"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	code, out, errOut := ygg(t, "metrics", ids[0]+".."+ids[2], "--project", dir)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, ids[1]) {
		t.Errorf("exit %d, stdout %q, stderr %q; want 2, nothing, and the middle run named", code, out, errOut)
	}
	sameTree(t, "a refused range", before, snapshot(t, dir))
}

func TestYggMetricsCompareRefusals(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	other := anotherRun(t, dir, config)
	before := snapshot(t, dir)
	for name, args := range map[string][]string{
		"a range argument": {"metrics", "--compare", id + ".." + other, other, "--project", dir},
		"one id":           {"metrics", "--compare", id, "--project", dir},
		"a missing id":     {"metrics", "--compare", id, "20200101-000000-aaaaaa", "--project", dir},
		"a traversal id":   {"metrics", "--compare", id, "../x", "--project", dir},
	} {
		code, out, errOut := ygg(t, args...)
		if code != ExitRefused || out != "" || errOut == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 2, empty stdout and a message", name, code, out, errOut)
		}
		sameTree(t, name, before, snapshot(t, dir))
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

func TestYggMetricsCompareOfTwoRealRunsPrintsTheDeltas(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	first, _ := walk(t, dir, config)
	second := anotherRun(t, dir, config)
	code, out, errOut := ygg(t, "metrics", "--compare", first, second, "--project", dir)
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"compare:", first, second, "comparable:   yes", "metric", "cost at API prices (usd)", "interventions", "reward:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the comparison lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = ygg(t, "metrics", "--compare", first, second, "--project", dir, "--json")
	var got struct {
		Comparable bool
		Rows       []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(out), &got); code != ExitDone || err != nil || !got.Comparable || len(got.Rows) == 0 {
		t.Errorf("--json: exit %d, %v, %+v", code, err, got)
	}
}
