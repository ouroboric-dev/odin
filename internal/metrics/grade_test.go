package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

func writeScore(t *testing.T, dir string) {
	t.Helper()
	s := gungnir.Score{
		Schema: gungnir.ScoreSchema, Run: testRun, Task: "client-gallery-lite", TaskVersion: "4.0.0",
		BriefSHA256: "bd4b2c7aaf532d51acb3a7edfd5b193d368db8445093b7a24cd80b7ee24d8319", Suite: "v2", Started: t0, Finished: t0.Add(20 * time.Minute),
		Scenarios: []gungnir.ScoredScenario{
			{ID: "photographer creates a gallery", Category: "photographer", Kind: gungnir.FailToPass, Status: gungnir.ScenarioPassed},
			{ID: "photographer uploads a photo", Category: "photographer", Kind: gungnir.FailToPass, Status: gungnir.ScenarioFailed},
			{ID: "client opens a link", Category: "client", Kind: gungnir.FailToPass, Status: gungnir.ScenarioPassed},
			{ID: "login page loads", Category: "client", Kind: gungnir.PassToPass, Status: gungnir.ScenarioFailed},
			{ID: "old link still works", Category: "client", Kind: gungnir.PassToPass, Status: gungnir.ScenarioFailed},
		},
		FailToPass: gungnir.Tally{Passed: 2, Total: 3}, PassToPass: gungnir.Tally{Passed: 0, Total: 2},
		Categories: map[string]gungnir.CategoryScore{"photographer": {Passed: 1, Total: 2, Weight: 0.4}, "client": {Passed: 1, Total: 1, Weight: 0.6}},
		Reward:     0.4*0.5 + 0.6,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.ParseScore(b); err != nil {
		t.Fatalf("the test's own score is invalid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "score.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gradedRun(t *testing.T, blocked bool) *Run {
	t.Helper()
	f := newFake(t).features(feature("g-T1", "repos/api", schema.TaskDone, ""), feature("g-T2", "repos/web", schema.TaskDone, ""))
	if blocked {
		f.State.Features[1] = feature("g-T2", "repos/web", schema.TaskBlocked, "attempts-exhausted")
	}
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(30, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	f.log(1000, gungnir.Event{Name: gungnir.EventGradeStarted, Task: "client-gallery-lite", Suite: "v2"})
	reward := 0.8
	f.log(1045, gungnir.Event{Name: gungnir.EventGradeFinished, Task: "client-gallery-lite", Suite: "v2", Reward: &reward, FailToPass: "2/3", PassToPass: "1/2"})
	dir := f.done()
	writeScore(t, dir)
	return mustCollect(t, dir)
}

func TestTheGradeComesFromScoreJSONWithItsCategoriesAndTheGradeTimeIsNotRunTime(t *testing.T) {
	r := gradedRun(t, false)
	if minutes(r.Time.Elapsed) != 30 || minutes(r.Time.Active) != 30 {
		t.Errorf("elapsed %v, active %v; want 30 minutes each: grading came a day later", r.Time.Elapsed, r.Time.Active)
	}
	g := r.Grade
	if g == nil {
		t.Fatal("Grade = nil")
	}
	if !near(g.Reward, 0.8) || g.FailToPass != (Tally{2, 3}) || g.PassToPass != (Tally{0, 2}) || g.Suite != "v2" || g.Task != "client-gallery-lite" || minutes(g.Duration) != 45 {
		t.Errorf("Grade = %+v", g)
	}
	if c := g.Categories["photographer"]; c.Passed != 1 || c.Total != 2 || !near(c.Weight, 0.4) {
		t.Errorf("category photographer = %+v", c)
	}
}

// Spec §10.2: escaped defects are hidden tests failing where the visible tests passed.
func TestEscapedDefectsAreFailedFailToPassScenariosAndSayWhetherTasksWereGivenUp(t *testing.T) {
	g := gradedRun(t, false).Grade
	if g.EscapedDefects != 1 || g.TasksGivenUp != 0 {
		t.Errorf("EscapedDefects = %d, TasksGivenUp = %d; want 1 and 0", g.EscapedDefects, g.TasksGivenUp)
	}
	g = gradedRun(t, true).Grade
	if g.EscapedDefects != 1 || g.TasksGivenUp != 1 {
		t.Errorf("EscapedDefects = %d, TasksGivenUp = %d; want 1 and 1: with a task given up some failures are unbuilt work", g.EscapedDefects, g.TasksGivenUp)
	}
}

func TestWithoutScoreJSONTheGradeIsTheLedgersAndARefusalIsSaid(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(5, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	reward := 0.25
	f.log(9, gungnir.Event{Name: gungnir.EventGradeStarted, Task: "t", Suite: "v1"})
	f.log(10, gungnir.Event{Name: gungnir.EventGradeFinished, Task: "t", Suite: "v1", Reward: &reward, FailToPass: "1/4", PassToPass: "2/2"})
	g := mustCollect(t, f.done()).Grade
	if g == nil || !near(g.Reward, 0.25) || g.FailToPass != (Tally{1, 4}) || g.EscapedDefects != 3 || len(g.Categories) != 0 {
		t.Errorf("Grade = %+v; want the ledger's reward 0.25, 1/4 fail-to-pass, 3 escaped, no categories", g)
	}
	r := newFake(t)
	r.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(5, gungnir.Event{Name: gungnir.EventRunFinished, Result: "done"})
	r.log(6, gungnir.Event{Name: gungnir.EventGradeRefused, Task: "t", Suite: "v1", Reason: gungnir.GradeBriefChanged})
	got := mustCollect(t, r.done())
	if got.Grade == nil || got.Grade.Refused != "brief_changed" {
		t.Errorf("Grade = %+v; want a refusal that says brief_changed", got.Grade)
	}
	none := newFake(t)
	none.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	if got := mustCollect(t, none.done()); got.Grade != nil {
		t.Errorf("a run never graded has Grade = %+v, want nil", got.Grade)
	}
}

func TestARoutingIndexReportsItsBenchAndAnUnmeasuredOneSaysSo(t *testing.T) {
	bench, base, bar := 0.9, 0.5, 0.8
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventRoutingIndexBuilt, IndexKind: "wiring", Edges: 4})
	f.log(1, gungnir.Event{Name: gungnir.EventRoutingLookup})
	f.log(2, gungnir.Event{Name: gungnir.EventRoutingIndexBuilt, IndexKind: "impact", Edges: 4, RoutingBench: &bench, RoutingBaseline: &base, RoutingThreshold: &bar})
	got := mustCollect(t, f.done()).Routing
	if got.Indexes != 2 || got.Lookups != 1 || got.Bench == nil || !near(*got.Bench, 0.9) || !near(*got.Baseline, 0.5) || !near(*got.Threshold, 0.8) {
		t.Errorf("Routing = %+v", got)
	}
	g := newFake(t)
	g.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, gungnir.Event{Name: gungnir.EventRoutingIndexBuilt, IndexKind: "wiring", Edges: 4})
	if got := mustCollect(t, g.done()).Routing; got.Indexes != 1 || got.Bench != nil {
		t.Errorf("Routing = %+v; an index shipped without a bench has no score", got)
	}
}
