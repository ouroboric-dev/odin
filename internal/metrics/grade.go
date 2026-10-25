package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// Tally is passed out of total scenarios.
type Tally struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// Category is one category of the reward.
type Category struct {
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
	Weight float64 `json:"weight"`
}

// GradeSkipped is the result of a grade node when factory.yaml has no grader section: Odin skips the node, and the
// run goes on with no grade (ruling R63).
const GradeSkipped = "graded: skipped, no grader in factory.yaml"

// Grade is the hidden suite's result for the run.
type Grade struct {
	Task        string              `json:"task"`
	TaskVersion string              `json:"task_version,omitempty"`
	Suite       string              `json:"suite"`
	Reward      float64             `json:"reward"`
	FailToPass  Tally               `json:"fail_to_pass"`
	PassToPass  Tally               `json:"pass_to_pass"`
	Categories  map[string]Category `json:"categories"`
	Duration    Duration            `json:"duration_ms"`
	// EscapedDefects is the number of fail-to-pass scenarios that failed. TasksGivenUp says how many of the
	// run's tasks were never built: with any, some of the failures are missing work and not an escape.
	EscapedDefects int    `json:"escaped_defects"`
	TasksGivenUp   int    `json:"tasks_given_up"`
	Refused        string `json:"refused,omitempty"` // grading was refused, and why
}

// Routing is what the routing indexes of the run report (spec §8).
type Routing struct {
	Lookups   int      `json:"lookups"` // routing_lookup lines; the factory does not write them yet
	Indexes   int      `json:"indexes"` // routing_index_built lines
	Bench     *float64 `json:"bench,omitempty"`
	Baseline  *float64 `json:"baseline,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// routing counts the lookups and reads the bench score of the last index that was measured.
func (r *Run) routing(events []gungnir.Event) {
	for _, e := range events {
		switch e.Name {
		case gungnir.EventRoutingLookup:
			r.Routing.Lookups++
		case gungnir.EventRoutingIndexBuilt:
			r.Routing.Indexes++
			if e.RoutingBench != nil {
				r.Routing.Bench, r.Routing.Baseline, r.Routing.Threshold = e.RoutingBench, e.RoutingBaseline, e.RoutingThreshold
			}
		}
	}
}

// grade reads score.json, or the grade_finished line when there is none, and the time grading took.
func (r *Run) grade(dir string, state *record, events []gungnir.Event) error {
	var started, finished time.Time
	var fin *gungnir.Event
	var refused string
	for i, e := range events {
		switch e.Name {
		case gungnir.EventGradeStarted:
			started, refused = e.Time, ""
		case gungnir.EventGradeFinished:
			finished, fin, refused = e.Time, &events[i], ""
		case gungnir.EventGradeRefused:
			refused = string(e.Reason)
		case gungnir.EventNodeFinished:
			r.GradeSkipped = r.GradeSkipped || e.Result == GradeSkipped
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "score.json"))
	switch {
	case err == nil:
		s, err := schema.ParseScore(data)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(dir, "score.json"), err)
		}
		g := &Grade{Task: s.Task, TaskVersion: s.TaskVersion, Suite: s.Suite, Reward: s.Reward,
			FailToPass: Tally(s.FailToPass), PassToPass: Tally(s.PassToPass), Categories: map[string]Category{}}
		for name, c := range s.Categories {
			g.Categories[name] = Category(c)
		}
		r.Grade = g
	case os.IsNotExist(err) && fin != nil && fin.Reward != nil:
		g := &Grade{Task: fin.Task, Suite: fin.Suite, Reward: *fin.Reward, Categories: map[string]Category{}}
		g.FailToPass, g.PassToPass = tally(fin.FailToPass), tally(fin.PassToPass)
		r.Grade = g
	case os.IsNotExist(err):
		if refused != "" {
			r.Grade = &Grade{Refused: refused, Categories: map[string]Category{}}
		}
		return nil
	default:
		return err
	}
	if !started.IsZero() && !finished.IsZero() && finished.After(started) {
		r.Grade.Duration = Duration(finished.Sub(started))
	}
	r.Grade.EscapedDefects = r.Grade.FailToPass.Total - r.Grade.FailToPass.Passed
	r.Grade.TasksGivenUp = len(r.BlockedTasks)
	return nil
}

// tally reads "passed/total".
func tally(s string) Tally {
	p, t, ok := strings.Cut(s, "/")
	if !ok {
		return Tally{}
	}
	a, _ := strconv.Atoi(p)
	b, _ := strconv.Atoi(t)
	return Tally{a, b}
}
