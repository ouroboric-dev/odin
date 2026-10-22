package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// metrics runs ygg metrics: the figures of spec §10.2 for one run, or for the runs between two ids (A..B). It
// reads the run's folder and writes nothing. --json prints the same figures as JSON, for a tool.
func (c *command) metrics(args []string) int {
	f, pos, ok := c.parse("metrics", args, 1, func(fs *flag.FlagSet, f *flags) {
		fs.BoolVar(&f.json, "json", false, "print JSON, not text")
	})
	if !ok {
		return ExitRefused
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	ids, err := runIDs(project, pos[0])
	if err != nil {
		return c.refuse(err)
	}
	var runs []*metrics.Run
	for _, id := range ids {
		r, err := metrics.Collect(run.Dir(project, id))
		if err != nil {
			return c.refuse(err)
		}
		runs = append(runs, r)
	}
	single := !strings.Contains(pos[0], "..")
	switch {
	case f.json && single:
		return c.printJSON(runs[0])
	case f.json:
		return c.printJSON(runs)
	case single:
		runs[0].WriteText(c.out)
	default:
		metrics.WriteRange(c.out, runs)
	}
	return ExitDone
}

func (c *command) printJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return c.refuse(err)
	}
	fmt.Fprintf(c.out, "%s\n", b)
	return ExitDone
}

// runIDs resolves "ID" to that run, which must exist, and "A..B" to every run of the project whose id is from A
// to B, in order. Ids start with the time the run began, so their order is the order the runs began in.
func runIDs(project, spec string) ([]string, error) {
	from, to, isRange := strings.Cut(spec, "..")
	if !isRange {
		if !run.ValidID(spec) {
			return nil, fmt.Errorf("%q is not a run id", spec)
		}
		if _, err := os.Stat(run.Dir(project, spec)); err != nil {
			return nil, fmt.Errorf("project %s has no run %s", project, spec)
		}
		return []string{spec}, nil
	}
	if !run.ValidID(from) || !run.ValidID(to) {
		return nil, fmt.Errorf("%q is not a range of two run ids (A..B)", spec)
	}
	if from > to {
		return nil, fmt.Errorf("the range %s is backwards: %s began after %s", spec, from, to)
	}
	entries, err := os.ReadDir(filepath.Join(project, ".factory", "runs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && run.ValidID(e.Name()) && e.Name() >= from && e.Name() <= to {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("project %s has no run from %s to %s", project, from, to)
	}
	slices.Sort(ids)
	return ids, nil
}
