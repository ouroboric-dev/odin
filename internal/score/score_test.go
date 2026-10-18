package score

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

var (
	t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	t1 = t0.Add(4 * time.Minute)
)

// manifest: two fail-to-pass in core, one in security, one pass-to-pass in core.
func input() Input {
	return Input{
		Run: "20261018-090000-a1b2c3", Task: "tiny-task", TaskVersion: "2.1.0", Suite: "v1",
		BriefSHA256: strings.Repeat("ab", 32), Started: t0, Finished: t1,
		Weights: map[string]float64{"core": 0.6, "security": 0.4},
		Manifest: gungnir.Suite{Schema: gungnir.SuiteSchema, Suite: "v1", Scenarios: []gungnir.Scenario{
			{ID: "a thing works", Category: "core", Kind: gungnir.FailToPass},
			{ID: "a thing stays", Category: "core", Kind: gungnir.PassToPass},
			{ID: "a second thing works", Category: "core", Kind: gungnir.FailToPass},
			{ID: "a thing is private", Category: "security", Kind: gungnir.FailToPass},
		}},
	}
}

func passed(ids ...string) map[string]gungnir.ScenarioStatus {
	m := map[string]gungnir.ScenarioStatus{}
	for _, id := range ids {
		m[id] = gungnir.ScenarioPassed
	}
	return m
}

func all(status gungnir.ScenarioStatus) map[string]gungnir.ScenarioStatus {
	m := passed("a thing works", "a thing stays", "a second thing works", "a thing is private")
	for id := range m {
		m[id] = status
	}
	return m
}

func TestComputeAddsUpTheRewardFromTheFailToPassScenariosOnly(t *testing.T) {
	res := passed("a thing works", "a thing stays", "a second thing works", "a thing is private")
	res["a second thing works"] = gungnir.ScenarioFailed
	s, err := Compute(input(), res)
	if err != nil {
		t.Fatalf("Compute() = %v", err)
	}
	if s.FailToPass != (gungnir.Tally{Passed: 2, Total: 3}) || s.PassToPass != (gungnir.Tally{Passed: 1, Total: 1}) {
		t.Errorf("tallies = %+v, %+v", s.FailToPass, s.PassToPass)
	}
	// core: 1 of 2 at 0.6, security: 1 of 1 at 0.4.
	if got := s.Categories["core"]; got != (gungnir.CategoryScore{Passed: 1, Total: 2, Weight: 0.6}) {
		t.Errorf("core = %+v", got)
	}
	if s.Reward < 0.7-1e-12 || s.Reward > 0.7+1e-12 {
		t.Errorf("reward = %v, want 0.7", s.Reward)
	}
	if s.Schema != gungnir.ScoreSchema || s.Run != input().Run || s.Task != "tiny-task" || s.TaskVersion != "2.1.0" || s.Suite != "v1" || s.BriefSHA256 != input().BriefSHA256 {
		t.Errorf("score = %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the score does not validate: %v", err)
	}
}

func TestPassToPassIsReportedOnItsOwnAndNeverChangesTheReward(t *testing.T) {
	res := all(gungnir.ScenarioPassed)
	res["a thing stays"] = gungnir.ScenarioFailed
	s, err := Compute(input(), res)
	if err != nil {
		t.Fatal(err)
	}
	if s.Reward < 1-1e-12 || s.PassToPass != (gungnir.Tally{Passed: 0, Total: 1}) {
		t.Errorf("reward %v, pass_to_pass %+v; a failed pass-to-pass scenario must show there and leave the reward at 1", s.Reward, s.PassToPass)
	}
}

func TestACategoryWithNoFailToPassScenarioCountsZero(t *testing.T) {
	in := input()
	in.Weights = map[string]float64{"core": 0.5, "security": 0.3, "billing": 0.2}
	s, err := Compute(in, all(gungnir.ScenarioPassed))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Categories["billing"]; got != (gungnir.CategoryScore{Weight: 0.2}) {
		t.Errorf("billing = %+v", got)
	}
	if s.Reward < 0.8-1e-12 || s.Reward > 0.8+1e-12 {
		t.Errorf("reward = %v, want 0.8: a category without scenarios scores nothing", s.Reward)
	}
}

func TestASkippedScenarioIsNotAPass(t *testing.T) {
	s, err := Compute(input(), all(gungnir.ScenarioSkipped))
	if err != nil {
		t.Fatal(err)
	}
	if s.Reward != 0 || s.FailToPass.Passed != 0 || s.Scenarios[0].Status != gungnir.ScenarioSkipped {
		t.Errorf("score = %+v", s)
	}
}

func TestAScenarioOnOneSideOnlyIsAnErrorNeverAPass(t *testing.T) {
	missing := all(gungnir.ScenarioPassed)
	delete(missing, "a thing is private")
	extra := all(gungnir.ScenarioPassed)
	extra["a new thing"] = gungnir.ScenarioPassed
	for name, c := range map[string]struct {
		res           map[string]gungnir.ScenarioStatus
		manifest, run string
	}{
		"a scenario the suite did not run":  {missing, "a thing is private", ""},
		"a spec the manifest does not know": {extra, "", "a new thing"},
		"results with nothing in them":      {map[string]gungnir.ScenarioStatus{}, "a thing works", ""},
	} {
		_, err := Compute(input(), c.res)
		var m *MismatchError
		if !errors.As(err, &m) {
			t.Errorf("%s: Compute() = %v, want a *MismatchError", name, err)
			continue
		}
		if c.manifest != "" && !strings.Contains(m.Error(), c.manifest) || c.run != "" && !strings.Contains(m.Error(), c.run) {
			t.Errorf("%s: %v does not name the scenario", name, m)
		}
	}
}

func TestComputeRefusesWhatCannotBeScored(t *testing.T) {
	in := input()
	in.Weights = map[string]float64{"core": 1}
	if _, err := Compute(in, all(gungnir.ScenarioPassed)); err == nil {
		t.Error("a fail-to-pass category with no weight scored")
	}
	in = input()
	in.Finished = t0.Add(-time.Second)
	if _, err := Compute(in, all(gungnir.ScenarioPassed)); err == nil {
		t.Error("a run that finished before it started scored")
	}
}

func TestScoreJSONIsTheContract(t *testing.T) {
	res := passed("a thing works", "a thing stays", "a second thing works", "a thing is private")
	res["a thing is private"] = gungnir.ScenarioFailed
	s, err := Compute(input(), res)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := Write(dir, s)
	if err != nil || path != filepath.Join(dir, "score.json") {
		t.Fatalf("Write() = %q, %v", path, err)
	}
	got, _ := os.ReadFile(path)
	want := `{
  "schema": "ygg.score/v1",
  "run": "20261018-090000-a1b2c3",
  "task": "tiny-task",
  "task_version": "2.1.0",
  "brief_sha256": "` + strings.Repeat("ab", 32) + `",
  "suite": "v1",
  "started": "2026-10-18T09:00:00Z",
  "finished": "2026-10-18T09:04:00Z",
  "scenarios": [
    { "id": "a thing works", "category": "core", "kind": "fail-to-pass", "status": "passed" },
    { "id": "a thing stays", "category": "core", "kind": "pass-to-pass", "status": "passed" },
    { "id": "a second thing works", "category": "core", "kind": "fail-to-pass", "status": "passed" },
    { "id": "a thing is private", "category": "security", "kind": "fail-to-pass", "status": "failed" }
  ],
  "fail_to_pass": { "passed": 2, "total": 3 },
  "pass_to_pass": { "passed": 1, "total": 1 },
  "categories": {
    "core": { "passed": 2, "total": 2, "weight": 0.6 },
    "security": { "passed": 0, "total": 1, "weight": 0.4 }
  },
  "reward": 0.6
}
`
	if !sameJSON(t, string(got), want) {
		t.Errorf("score.json:\n%s\nwant (as JSON):\n%s", got, want)
	}
}

func TestWriteRefusesARunFolderThatIsNotAbsoluteAndWritesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("run", 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := Compute(input(), all(gungnir.ScenarioPassed))
	if _, err := Write("run", s); err == nil {
		t.Error("Write(\"run\") = nil: a relative folder means whatever the working directory is")
	}
	if left, _ := os.ReadDir("run"); len(left) != 0 {
		t.Errorf("a refused write left %d file(s) in run/", len(left))
	}
}

func TestWriteMakesOneFileInTheRunFolderAndNothingBesideIt(t *testing.T) {
	parent := t.TempDir()
	runDir := filepath.Join(parent, "run")
	os.Mkdir(runDir, 0o755)
	s, _ := Compute(input(), all(gungnir.ScenarioPassed))
	if _, err := Write(runDir, s); err != nil {
		t.Fatal(err)
	}
	var found []string
	filepath.WalkDir(parent, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(parent, p)
			found = append(found, rel)
		}
		return nil
	})
	if strings.Join(found, ",") != filepath.Join("run", "score.json") {
		t.Errorf("files = %v, want only run/score.json", found)
	}
	if st, _ := os.Stat(filepath.Join(runDir, "score.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	bad := s
	bad.Reward = 1.5
	if _, err := Write(runDir, bad); err == nil {
		t.Error("Write() wrote a score that does not add up")
	}
	for _, dir := range []string{"relative/run", filepath.Join(parent, "none")} {
		if _, err := Write(dir, s); err == nil {
			t.Errorf("Write(%q) = nil", dir)
		}
	}
}

// sameJSON compares two documents by value, so the test does not depend on white space.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("score.json is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
