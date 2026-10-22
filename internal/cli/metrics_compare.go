package cli

import (
	"flag"
	"path/filepath"

	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// compare runs ygg metrics --compare <runA> <runB>: what differed between two runs and the deltas (spec §10.2).
func (c *command) compare(args []string) int {
	f, pos, ok := c.parse("metrics --compare", args, 2, func(fs *flag.FlagSet, f *flags) {
		fs.BoolVar(&f.json, "json", false, "print JSON, not text")
	})
	if !ok {
		return ExitRefused
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	var runs [2]*metrics.Run
	for i, id := range pos {
		ids, err := runIDs(project, id)
		if err != nil || len(ids) != 1 || id != ids[0] {
			if err == nil {
				err = errNotOneRun(id)
			}
			return c.refuse(err)
		}
		if runs[i], err = metrics.Collect(run.Dir(project, id)); err != nil {
			return c.refuse(err)
		}
	}
	cmp := metrics.Compare(runs[0], runs[1])
	if f.json {
		return c.printJSON(cmp)
	}
	cmp.WriteText(c.out)
	return ExitDone
}
