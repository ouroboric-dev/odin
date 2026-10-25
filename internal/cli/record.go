package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// record runs ygg record: it rebuilds the run's experiment record (ygg.run-record/v1) from the ledger, run.json
// and the files the run kept, writes it to record.json in the run's folder, and prints its path. It can run at
// any time, again and again: the same run gives the same record. A run that another ygg process is advancing has its
// record printed instead, and not written: that process writes record.json when it stops, and a write here could
// land between its rebuild and its rename.
func (c *command) record(args []string) int {
	f, pos, ok := c.parse("record", args, 1, nil)
	if !ok {
		return ExitRefused
	}
	if strings.Contains(pos[0], "..") {
		return c.refuse(fmt.Errorf("%q is not one run id: ygg record takes one run", pos[0]))
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	ids, err := runIDs(project, pos[0])
	if err != nil {
		return c.refuse(err)
	}
	dir := run.Dir(project, ids[0])
	unlock, err := run.Lock(dir)
	if errors.Is(err, run.ErrInUse) {
		r, err := metrics.Collect(dir)
		if err != nil {
			return c.refuse(err)
		}
		b, err := metrics.Encode(r)
		if err != nil {
			return c.refuse(err)
		}
		fmt.Fprint(c.out, string(b))
		fmt.Fprintf(c.err, "ygg: run %s is in use; its record.json is written when it stops\n", ids[0])
		return ExitDone
	}
	if err != nil {
		return c.refuse(err)
	}
	defer unlock()
	path, err := metrics.WriteRecord(dir)
	if err != nil {
		return c.refuse(err)
	}
	fmt.Fprintln(c.out, path)
	return ExitDone
}
