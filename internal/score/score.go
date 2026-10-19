package score

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

// FileName is the name of the score in the run folder.
const FileName = "score.json"

// Input is what a score is made of, besides the results: the task and the suite it was graded by.
type Input struct {
	Run         string // the run's id
	Task        string
	TaskVersion string
	Suite       string
	BriefSHA256 string
	Manifest    gungnir.Suite      // the suite's manifest, which Load has validated
	Weights     map[string]float64 // the categories of the reward; they sum to 1
	Started     time.Time
	Finished    time.Time
}

// Compute joins the manifest with the results and adds up the score (contract: reward = sum over the
// categories of weight * passed / total, fail-to-pass scenarios only, a category with none counts 0;
// pass-to-pass is reported on its own). The scenarios keep the manifest's order. The categories are
// summed in name order, so the same results always give the same bytes.
func Compute(in Input, results map[string]gungnir.ScenarioStatus) (gungnir.Score, error) {
	if err := in.Manifest.Validate(); err != nil {
		return gungnir.Score{}, err
	}
	var miss MismatchError
	known := map[string]bool{}
	for _, sc := range in.Manifest.Scenarios {
		known[sc.ID] = true
		if _, ok := results[sc.ID]; !ok {
			miss.NotRun = append(miss.NotRun, sc.ID)
		}
	}
	for id := range results {
		if !known[id] {
			miss.Unknown = append(miss.Unknown, id)
		}
	}
	if len(miss.NotRun)+len(miss.Unknown) > 0 {
		slices.Sort(miss.Unknown)
		return gungnir.Score{}, &miss
	}
	s := gungnir.Score{Schema: gungnir.ScoreSchema, Run: in.Run, Task: in.Task, TaskVersion: in.TaskVersion, BriefSHA256: in.BriefSHA256,
		Suite: in.Suite, Started: in.Started, Finished: in.Finished, Categories: map[string]gungnir.CategoryScore{}}
	for name, w := range in.Weights {
		s.Categories[name] = gungnir.CategoryScore{Weight: w}
	}
	for _, sc := range in.Manifest.Scenarios {
		st := results[sc.ID]
		s.Scenarios = append(s.Scenarios, gungnir.ScoredScenario{ID: sc.ID, Category: sc.Category, Kind: sc.Kind, Status: st})
		tally := &s.PassToPass
		if sc.Kind == gungnir.FailToPass {
			c, ok := s.Categories[sc.Category]
			if !ok {
				return gungnir.Score{}, fmt.Errorf("scenario %q is in category %s, which has no weight", sc.ID, sc.Category)
			}
			c.Total++
			if st == gungnir.ScenarioPassed {
				c.Passed++
			}
			s.Categories[sc.Category] = c
			tally = &s.FailToPass
		}
		tally.Total++
		if st == gungnir.ScenarioPassed {
			tally.Passed++
		}
	}
	names := make([]string, 0, len(s.Categories))
	for name := range s.Categories {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if c := s.Categories[name]; c.Total > 0 {
			s.Reward += c.Weight * float64(c.Passed) / float64(c.Total)
		}
	}
	return s, s.Validate()
}

// Write validates the score and writes it as score.json in runDir: atomically, readable by the operator
// only. runDir is the run's own folder; Write has no other place to write to, and refuses a relative
// path or a folder that is not there. It returns the path.
func Write(runDir string, s gungnir.Score) (string, error) {
	if !filepath.IsAbs(runDir) {
		return "", fmt.Errorf("the run folder %q must be an absolute path", runDir)
	}
	if err := s.Validate(); err != nil {
		return "", fmt.Errorf("the score does not add up: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(runDir, ".score-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // gone after the rename; a failed write leaves nothing
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(runDir, FileName)
	return path, os.Rename(tmp.Name(), path)
}
