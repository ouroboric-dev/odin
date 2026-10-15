package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
)

// doctor checks the config and every run in the project, and prints one line per finding. It
// exits 1 when anything is an ERROR. Phase 3 adds leftover containers and processes.
func (c *command) doctor(args []string) int {
	f, _, ok := c.parse("doctor", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
	})
	if !ok {
		return ExitRefused
	}
	bad := false
	report := func(level, what, msg string) {
		fmt.Fprintf(c.out, "%-5s %s: %s\n", level, what, msg)
		if level == "ERROR" {
			bad = true
		}
	}
	if _, err := schema.LoadFactory(f.config); err != nil {
		var ce *schema.CapsError
		if errors.As(err, &ce) {
			report("ERROR", "caps", err.Error())
		} else {
			report("ERROR", "config", err.Error())
		}
	} else {
		report("OK", "config", f.config)
	}
	runs := filepath.Join(f.project, ".factory", "runs")
	entries, err := os.ReadDir(runs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		report("ERROR", "runs", err.Error())
	}
	for _, e := range entries {
		dir := filepath.Join(runs, e.Name())
		if !e.IsDir() || !run.ValidID(e.Name()) {
			report("WARN", "runs", e.Name()+" is not a run")
			continue
		}
		s, err := run.LoadState(dir)
		if err != nil {
			report("ERROR", "run "+e.Name(), err.Error())
			continue
		}
		if _, err := ledger.Verify(filepath.Join(dir, "ledger.jsonl"), s.Ledger); err != nil {
			report("ERROR", "run "+s.ID, "ledger: "+err.Error())
			continue
		}
		if s.Status == run.StatusRunning && !run.Locked(dir) {
			report("WARN", "run "+s.ID, "interrupted at node position "+fmt.Sprint(s.Position)+"; ygg resume "+s.ID)
			continue
		}
		report("OK", "run "+s.ID, string(s.Status))
	}
	if bad {
		return ExitFailed
	}
	return ExitDone
}
