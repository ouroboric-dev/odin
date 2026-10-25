package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// scoreClock is the clock of ygg score, which a test replaces.
var scoreClock = time.Now

// writeJudgements writes judgements.json for ygg score; a test replaces it with one that fails.
var writeJudgements = metrics.WriteJudgements

// score runs ygg score: the operator's judgement of a run, or with --task of one of its tasks, as a score from 0 to
// 10 and what went well and what went wrong (ygg.judgement/v1). The new judgement replaces the last one of the run
// or the task in judgements.json, the ledger keeps each as a judged line, and record.json is rebuilt with it. It can
// be given at any time after the run started, a run that stopped or is not done included. A run that another ygg
// process is advancing is refused: that process owns the ledger.
func (c *command) score(args []string) int {
	f, pos, ok := c.parse("score", args, 1, func(fs *flag.FlagSet, f *flags) {
		fs.IntVar(&f.score, "score", -1, "the score, a whole number from 0 to 10 (10 is best)")
		fs.StringVar(&f.well, "well", "", "what went well, 1 to 16384 characters")
		fs.StringVar(&f.wrong, "wrong", "", "what went wrong, 1 to 16384 characters")
		fs.StringVar(&f.task, "task", "", "judge this task of the plan instead of the whole run")
	})
	if !ok {
		return ExitRefused
	}
	if strings.Contains(pos[0], "..") {
		return c.refuse(fmt.Errorf("%q is not one run id: ygg score takes one run", pos[0]))
	}
	if !f.set["score"] {
		return c.refuse(errors.New("score needs --score, a whole number from 0 to 10"))
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	ids, err := runIDs(project, pos[0])
	if err != nil {
		return c.refuse(err)
	}
	id, dir := ids[0], run.Dir(project, ids[0])
	now := scoreClock().UTC().Truncate(time.Second)
	j := schema.Judgement{Run: id, Task: f.task, Score: f.score, Well: f.well, Wrong: f.wrong, Time: now.Format(time.RFC3339)}
	b, err := json.Marshal(j)
	if err != nil {
		return c.refuse(err)
	}
	if j, err = schema.Decode[schema.Judgement](schema.ContractJudgement, b); err != nil {
		return c.refuse(fmt.Errorf("the judgement is not valid: %w", err))
	}
	unlock, err := run.Lock(dir)
	if err != nil {
		if errors.Is(err, run.ErrInUse) {
			return c.refuse(fmt.Errorf("run %s is in use: another ygg process is advancing it and owns its ledger; judge it when that process stops", id))
		}
		return c.refuse(err)
	}
	defer unlock()
	st, err := run.LoadState(dir)
	if err != nil {
		return c.refuse(err)
	}
	e := gungnir.Event{Name: gungnir.EventJudged, Result: strconv.Itoa(j.Score), Time: now}
	if j.Task != "" {
		i := slices.IndexFunc(st.Features, func(r schema.FeatureRecord) bool { return r.ID == j.Task })
		if i < 0 {
			return c.refuse(fmt.Errorf("the plan of run %s has no task %q", id, j.Task))
		}
		e.Task = st.Features[i].Slug()
	}
	old, err := metrics.ReadJudgements(dir)
	if err != nil {
		return c.refuse(err)
	}
	// The ledger first: a judgement in judgements.json always has its line, and one the ledger holds without the file
	// is given again by running the command again.
	if err := run.LogOutside(dir, e); err != nil {
		return c.refuse(err)
	}
	same := func(x schema.Judgement) bool { return x.Task == j.Task }
	if i := slices.IndexFunc(old, same); i >= 0 {
		old[i] = j
	} else {
		old = append(old, j)
	}
	if err := writeJudgements(dir, old); err != nil {
		// The ledger holds the judgement already: running score again would log it twice.
		fmt.Fprintf(c.err, "ygg: the judgement is in the ledger, but judgements.json could not be written: %v\nRunning ygg score again would log it twice.\n", err)
		return ExitFailed
	}
	path, err := metrics.WriteRecord(dir)
	if err != nil {
		// The judgement is in the ledger and in judgements.json: running score again would log it twice.
		fmt.Fprintf(c.err, "ygg: the judgement was recorded (judgements.json and the ledger), but record.json could not be rebuilt: %v\nRun: ygg record %s\n", err, id)
		return ExitFailed
	}
	what := "run " + id
	if j.Task != "" {
		what = "task " + j.Task + " of run " + id
	}
	fmt.Fprintf(c.out, "Judged %s: %d/10.\n%s\n", what, j.Score, path)
	return ExitDone
}
