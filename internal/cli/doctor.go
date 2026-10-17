package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
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
	var factory *schema.Factory
	if !rootOnly || f.set["config"] {
		factory = checkConfig(f.config, report)
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
	if factory != nil {
		checkBudget(factory, report)
		c.checkSandbox(f.project, factory, report)
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
		if s.Exec != nil && !run.Locked(dir) && run.LeftoverProcess(s.Exec) {
			if err := run.StopLeftoverProcess(s.Exec); err != nil {
				report("ERROR", "run "+s.ID, err.Error())
			} else {
				report("WARN", "run "+s.ID, fmt.Sprintf("node %s left process group %d running; stopped it", s.Exec.Node, s.Exec.PGID))
			}
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

func checkConfig(path string, report func(level, what, msg string)) *schema.Factory {
	f, err := schema.LoadFactory(path)
	if err != nil {
		var ce *schema.CapsError
		if errors.As(err, &ce) {
			report("ERROR", "caps", err.Error())
		} else {
			report("ERROR", "config", err.Error())
		}
		return nil
	}
	report("OK", "config", path)
	return f
}

// checkBudget says what the caps are and how much of today's is used, and checks that the spend
// journal can be read and written: a spend that can't be recorded can't be capped (spec §7.4).
func checkBudget(f *schema.Factory, report func(level, what, msg string)) {
	j, err := run.BudgetJournal(f, nil)
	if err != nil {
		report("ERROR", "budget", err.Error())
		return
	}
	if err := os.MkdirAll(j.Dir, 0o700); err != nil {
		report("ERROR", "budget", "the spend journal can't be made: "+err.Error())
		return
	}
	probe, err := os.CreateTemp(j.Dir, ".doctor-*")
	if err != nil {
		report("ERROR", "budget", "the spend journal folder isn't writable: "+err.Error())
		return
	}
	probe.Close()
	os.Remove(probe.Name())
	if err := j.Appendable(); err != nil {
		report("ERROR", "budget", "today's spend journal file can't be opened for appending: "+err.Error())
		return
	}
	day, err := j.Today()
	if err != nil {
		report("ERROR", "budget", err.Error())
		return
	}
	report("OK", "budget", describeCaps(f, gungnir.Usage{}, day)+" (journal "+j.Dir+")")
}

// checkSandbox checks the engine and the sandboxes of the project's runs (spec §6.2, §12): the
// engine answers and is rootless, the devcontainer CLI is the version the factory was tested with,
// and no container a killed ygg left is still running. It stops what it finds, because a run that no
// process holds will never stop it.
func (c *command) checkSandbox(project string, f *schema.Factory, report func(level, what, msg string)) {
	for _, r := range f.Roles {
		if r.Agent == "claude-code" && os.Getenv("ANTHROPIC_API_KEY") != "" {
			report("WARN", "env", "ANTHROPIC_API_KEY is set in this shell; claude-code sessions never receive it, and bill the subscription")
			break
		}
	}
	if f.Sandbox == nil {
		report("WARN", "sandbox", "no sandbox section: leaf commands run on the host, and checkpoints aren't scanned for secrets (only scripted roles can run)")
		return
	}
	work, err := os.MkdirTemp("", "ygg-doctor-")
	if err != nil {
		report("ERROR", "sandbox", err.Error())
		return
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(f.Sandbox.Engine, sandbox.Config{}, work)
	if err != nil {
		report("ERROR", "sandbox", err.Error())
		return
	}
	info, err := d.Info(c.ctx)
	switch {
	case err != nil:
		report("ERROR", "engine", fmt.Sprintf("%s doesn't answer: %v", f.Sandbox.Engine, err))
		return
	case !info.Rootless:
		report("ERROR", "engine", fmt.Sprintf("%s is a rootful engine: a container escape would land as root there (sandbox spike)", f.Sandbox.Engine))
		return
	}
	report("OK", "engine", fmt.Sprintf("podman %s, rootless, at %s", info.Podman, f.Sandbox.Engine))
	if v, err := d.DevcontainerCLI(c.ctx); err != nil {
		report("ERROR", "devcontainer", err.Error())
	} else if v != sandbox.DevcontainerVersion {
		report("WARN", "devcontainer", fmt.Sprintf("CLI %s; the factory was tested with %s", v, sandbox.DevcontainerVersion))
	} else {
		report("OK", "devcontainer", "CLI "+v)
	}
	left, err := d.Leftovers(c.ctx)
	if err != nil {
		report("ERROR", "sandboxes", err.Error())
		return
	}
	stopped := map[string]bool{}
	for _, l := range left {
		dir := run.Dir(project, l.Run)
		switch _, err := os.Stat(dir); {
		case err != nil:
			report("WARN", "sandboxes", fmt.Sprintf("container %.12s of run %s belongs to another project; doctor in that project stops it", l.Container, l.Run))
		case run.Locked(dir):
			// A live ygg owns it.
		case stopped[l.Project]:
		default:
			stopped[l.Project] = true
			if err := d.StopLeftover(c.ctx, l); err != nil {
				report("ERROR", "sandboxes", fmt.Sprintf("couldn't stop %s of run %s: %v", l.Project, l.Run, err))
			} else {
				report("WARN", "sandboxes", fmt.Sprintf("run %s left the sandbox of %s (%s); stopped it", l.Run, l.Leaf, l.State))
			}
		}
	}
	report("OK", "sandboxes", fmt.Sprintf("%d container(s) of ygg runs", len(left)))
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
