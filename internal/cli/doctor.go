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
	"github.com/ouroboric-dev/odin/internal/tree"
)

// doctor checks the config and every run in the project, and prints one line per finding. It
// exits 1 when anything is an ERROR. Phase 3 adds leftover containers and processes.
func (c *command) doctor(args []string) int {
	f, _, ok := c.parse("doctor", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
		fs.StringVar(&f.root, "root", "", "a tree's root to check, without a project")
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
	rootOnly := f.set["root"] && !f.set["project"] // an instance root alone: its tree, no runs
	if !rootOnly || f.set["config"] {
		checkConfig(f.config, report)
	}
	root := f.root
	if root == "" {
		var err error
		if root, _, err = tree.FindRoot(c.ctx, f.project); err != nil {
			report("ERROR", "tree", err.Error())
		}
	}
	if root != "" {
		c.checkTree(root, report)
	}
	if rootOnly {
		return exit(bad)
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
	return exit(bad)
}

func exit(bad bool) int {
	if bad {
		return ExitFailed
	}
	return ExitDone
}

func checkConfig(path string, report func(level, what, msg string)) {
	if _, err := schema.LoadFactory(path); err != nil {
		var ce *schema.CapsError
		if errors.As(err, &ce) {
			report("ERROR", "caps", err.Error())
		} else {
			report("ERROR", "config", err.Error())
		}
		return
	}
	report("OK", "config", path)
}

// checkTree walks the instance tree from root, checking out nothing, and warns when a public
// umbrella lists a private child: its name shows in .gitmodules (spec §3.1).
func (c *command) checkTree(root string, report func(level, what, msg string)) {
	nodes, present := 0, 0
	err := tree.Walk(c.ctx, root, func(n tree.Node) error {
		where := n.Path
		if where == "" {
			where = "."
		}
		nodes++
		if n.Present {
			present++
		}
		if n.Umbrella != nil {
			for _, p := range n.Umbrella.Leaks() {
				report("WARN", "tree", fmt.Sprintf("the public umbrella %s lists the private child %s; its name shows in .gitmodules", where, p))
			}
		}
		return nil
	})
	if err != nil {
		report("ERROR", "tree", err.Error())
		return
	}
	report("OK", "tree", fmt.Sprintf("%s: %d nodes, %d checked out", root, nodes, present))
}
