package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/realm"
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
	settled := false // no containers of a bump hold a product: git in a product is safe only when this is true
	if factory != nil {
		checkRealms(factory, report)
		checkGraphs(factory, report)
		checkBudget(factory, report)
		checkPrices(factory, report)
		checkGrader(factory, report)
		settled = c.checkSandbox(f.project, factory, report)
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
		if exec, ok := run.BeforeGraphs(dir); ok {
			c.checkRunBeforeGraphs(dir, exec, report)
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
		if s.Outputs == nil {
			report("WARN", "run "+s.ID, "started before typed boundaries ("+string(s.Status)+"), so it can't be resumed; status, report and metrics still read it")
			continue
		}
		if s.Status == run.StatusRunning && !run.Locked(dir) {
			report("WARN", "run "+s.ID, "interrupted at node position "+fmt.Sprint(s.Position)+"; ygg resume "+s.ID)
			continue
		}
		report("OK", "run "+s.ID, string(s.Status))
	}
	ids, msgs, err := c.unfinishedBumps(f.project, settled, "The sandbox containers of the bump are not known to be stopped, so git is not safe in the product yet. Once the engine answers, run: ygg doctor --project "+f.project)
	if err != nil {
		report("ERROR", "bumps", err.Error())
	}
	for i := range ids {
		report("WARN", "bump "+ids[i], msgs[i])
	}
	return exit(bad)
}

// checkRunBeforeGraphs reports a run that a realm led (ygg.run/v2) as a warning: it can't be resumed, so
// it is not an error, and its folder can go once no older ygg holds it (then doctor says the older ygg is still running it). A process group its exec node left is still stopped, from
// the two fields of run.json that name it; a leftover container is found by its label and stopped by
// checkSandbox, whatever the run's state.
func (c *command) checkRunBeforeGraphs(dir string, exec *run.ExecProcess, report func(level, what, msg string)) {
	what := "run " + filepath.Base(dir)
	if exec != nil {
		if err := exec.Check(); err != nil {
			report("ERROR", what, err.Error())
			exec = nil
		}
	}
	if exec != nil && !run.Locked(dir) && run.LeftoverProcess(exec) {
		if err := run.StopLeftoverProcess(exec); err != nil {
			report("ERROR", what, err.Error())
		} else {
			report("WARN", what, fmt.Sprintf("node %s left process group %d running; stopped it", exec.Node, exec.PGID))
		}
	}
	if run.Locked(dir) {
		report("WARN", what, "an older ygg is still running this run (schema ygg.run/v2); this ygg can't resume it")
		return
	}
	report("WARN", what, "this run is from before graphs (schema ygg.run/v2) and can't be resumed; remove its folder "+dir+" when you no longer need its ledger")
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

// checkRealms says which realm leaves the store holds and at which versions, and that each loads: a realm
// a project pins must load at the pinned version, or its runs stop before they start (change
// realm-leaf-naming). A config with no realms section is fine: a project just can't pin a realm leaf.
func checkRealms(f *schema.Factory, report func(level, what, msg string)) {
	if f.Realms == nil {
		report("WARN", "realms", "no realms section: a project can't pin a realm leaf")
		return
	}
	entries, err := os.ReadDir(f.Realms.Dir)
	if err != nil {
		report("ERROR", "realms", err.Error())
		return
	}
	var found []string
	for _, e := range entries {
		dir := filepath.Join(f.Realms.Dir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "pack.yaml")); err != nil {
			continue // not a realm leaf
		}
		r, err := realm.Load(dir)
		if err != nil {
			report("ERROR", "realms", e.Name()+": "+err.Error())
			continue
		}
		if r.Name != e.Name() {
			report("ERROR", "realms", fmt.Sprintf("%s holds the realm %s, so a project can't pin %s", dir, r.Name, e.Name()))
			continue
		}
		found = append(found, r.Name+" "+r.Version)
	}
	if len(found) == 0 {
		report("ERROR", "realms", f.Realms.Dir+" holds no realm leaf")
		return
	}
	report("OK", "realms", f.Realms.Dir+" ("+strings.Join(found, ", ")+")")
}

// checkGraphs says which graph packs the store holds and at which versions, and that each loads and holds a
// graph: --graph NAME finds a pack there, and a run takes its graph from it. A pack with no graph in the
// store is an error, since a run named for it would refuse. A config with no graphs section is fine: --graph
// takes a path, and a project can't pin a graph. When graphs.dir is realms.dir (ruling R16), a pack with no
// graph is a realm, not a broken graph pack: it is left out, and the store is an error only when it holds
// no graph pack at all.
func checkGraphs(f *schema.Factory, report func(level, what, msg string)) {
	if f.Graphs == nil {
		report("WARN", "graphs", "no graphs section: --graph takes a path, and a project can't pin a graph")
		return
	}
	entries, err := os.ReadDir(f.Graphs.Dir)
	if err != nil {
		report("ERROR", "graphs", err.Error())
		return
	}
	shared := f.Realms != nil && sameFolder(f.Realms.Dir, f.Graphs.Dir)
	var found []string
	for _, e := range entries {
		dir := filepath.Join(f.Graphs.Dir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "pack.yaml")); err != nil {
			continue // not a pack
		}
		r, err := realm.Load(dir)
		switch {
		case err == nil && r.Name == e.Name() && r.Pack.Graph == "" && shared:
			// a realm: checkRealms reports on it
		case err != nil:
			report("ERROR", "graphs", e.Name()+": "+err.Error())
		case r.Name != e.Name():
			report("ERROR", "graphs", fmt.Sprintf("%s holds the graph %s, so --graph %s can't find it", dir, r.Name, e.Name()))
		case r.Pack.Graph == "":
			report("ERROR", "graphs", e.Name()+": the pack holds no graph, so --graph "+e.Name()+" would be refused")
		default:
			found = append(found, r.Name+" "+r.Version)
		}
	}
	if len(found) == 0 {
		report("ERROR", "graphs", f.Graphs.Dir+" holds no graph pack")
		return
	}
	report("OK", "graphs", f.Graphs.Dir+" ("+strings.Join(found, ", ")+")")
}

// sameFolder says whether a and b are one folder, by their paths or by what they resolve to.
func sameFolder(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
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

// checkPrices warns of an OpenCode role on a Claude model through OpenRouter that has no price. OpenRouter
// reports a cost of 0 for those models, so the session adds no dollars to per_run.usd or per_day.usd, and
// gleipnir refuses it after the tokens are spent. A warning, not an error: the factory may not use the role.
func checkPrices(f *schema.Factory, report func(level, what, msg string)) {
	for _, name := range slices.Sorted(maps.Keys(f.Roles)) {
		r := f.Roles[name]
		if r.Agent == "opencode" && r.Price == nil && strings.HasPrefix(r.Model, "openrouter/anthropic/") {
			report("WARN", "price", fmt.Sprintf("role %s uses %s, which OpenRouter reports at cost 0; set roles.%s.price in factory.yaml or its sessions are refused and the dollar caps cannot count them", name, r.Model, name))
		}
	}
}

// checkSandbox checks the engine and the sandboxes of the project's runs (spec §6.2, §12): the
// engine answers and is rootless, the devcontainer CLI is the version the factory was tested with,
// and no container a killed ygg left is still running. It stops what it finds, because a run that no
// process holds will never stop it.
func (c *command) checkSandbox(project string, f *schema.Factory, report func(level, what, msg string)) (settled bool) {
	for _, r := range f.Roles {
		if r.Agent == "claude-code" && os.Getenv("ANTHROPIC_API_KEY") != "" {
			report("WARN", "env", "ANTHROPIC_API_KEY is set in this shell; claude-code sessions never receive it, and bill the subscription")
			break
		}
	}
	if f.Sandbox == nil {
		report("WARN", "sandbox", "no sandbox section: leaf commands run on the host, and checkpoints aren't scanned for secrets (only scripted roles can run)")
		return true // no container of a bump can hold a product
	}
	work, err := os.MkdirTemp("", "ygg-doctor-")
	if err != nil {
		report("ERROR", "sandbox", err.Error())
		return false
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(f.Sandbox.Engine, sandbox.Config{}, work)
	if err != nil {
		report("ERROR", "sandbox", err.Error())
		return false
	}
	info, err := d.Info(c.ctx)
	switch {
	case err != nil:
		report("ERROR", "engine", fmt.Sprintf("%s doesn't answer: %v", f.Sandbox.Engine, err))
		return false
	case !info.Rootless:
		report("ERROR", "engine", fmt.Sprintf("%s is a rootful engine: a container escape would land as root there (sandbox spike)", f.Sandbox.Engine))
		return false
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
		return false
	}
	stopped := map[string]bool{}
	settled = true // every container of a bump listed above is stopped, or there is none
	for _, l := range left {
		dir := run.Dir(project, l.Run)
		if isBump(project, l.Run) { // a bump names its sandbox by its own id; a live bump holds the runs lock
			switch {
			case bumpRunning(project), stopped[l.Project]:
			default:
				stopped[l.Project] = true
				if err := d.StopLeftover(c.ctx, l); err != nil {
					report("ERROR", "sandboxes", fmt.Sprintf("couldn't stop %s of bump %s: %v", l.Project, l.Run, err))
					settled = false
				} else {
					report("WARN", "sandboxes", fmt.Sprintf("bump %s left the sandbox of %s (%s); stopped it. %s", l.Run, l.Leaf, l.State, c.nextForProduct(project, l.Leaf, l.Run)))
				}
			}
			continue
		}
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
	c.checkPods(d, project, report)
	if f.Grader != nil {
		c.checkGraders(d, project, report)
	}
	return settled
}

// checkPods finds the empty pods that compose left when a sandbox went (backlog B-138) and removes the ones
// no live run holds. A pod has no label, so it is tied to a run by its project name starting with the
// run's prefix: a pod of a run or bump that a live ygg holds is left, since a sandbox that is still
// starting has an empty pod for a moment. Nothing is said when there is no empty pod. A pod that can't
// be removed is a warning, not an error: it does no harm.
func (c *command) checkPods(d *sandbox.Driver, project string, report func(level, what, msg string)) {
	left, err := d.PodLeftovers(c.ctx)
	if err != nil {
		report("ERROR", "pods", err.Error())
		return
	}
	if len(left) == 0 {
		return
	}
	ids := slices.Concat(dirNames(filepath.Join(project, ".factory", "runs")), dirNames(filepath.Join(project, ".factory", "deps")))
	for _, l := range left {
		i := slices.IndexFunc(ids, func(id string) bool { return strings.HasPrefix(l.Project, sandbox.RunPrefix(id)) })
		if i < 0 {
			report("WARN", "pods", fmt.Sprintf("pod %s belongs to another project; doctor in that project removes it", l.Pod))
			continue
		}
		id := ids[i]
		if isBump(project, id) && bumpRunning(project) || !isBump(project, id) && run.Locked(run.Dir(project, id)) {
			continue // a live ygg owns it
		}
		if err := d.RemovePod(c.ctx, l); err != nil {
			report("WARN", "pods", fmt.Sprintf("couldn't remove %s of run %s: %v", l.Pod, id, err))
		} else {
			report("WARN", "pods", fmt.Sprintf("run %s left the empty pod %s (%s); removed it", id, l.Pod, l.State))
		}
	}
	report("OK", "pods", fmt.Sprintf("%d empty pod(s) of ygg runs", len(left)))
}

// dirNames lists the folders in dir, or none when it can't be read.
func dirNames(dir string) (names []string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// checkGrader says how many tasks the grader's folder holds, and that its two folders are there.
func checkGrader(f *schema.Factory, report func(level, what, msg string)) {
	g := f.Grader
	if g == nil {
		return
	}
	tasks, err := os.ReadDir(g.TasksDir)
	if err != nil {
		report("ERROR", "grader", "tasks_dir: "+err.Error())
		return
	}
	n := 0
	for _, e := range tasks {
		if _, err := os.Stat(filepath.Join(g.TasksDir, e.Name(), "task.toml")); err == nil {
			n++
		}
	}
	if st, err := os.Stat(g.SuiteDir); err != nil || !st.IsDir() {
		report("ERROR", "grader", "suite_dir "+g.SuiteDir+" is not a folder")
		return
	}
	report("OK", "grader", fmt.Sprintf("%d task(s) in %s, suites in %s", n, g.TasksDir, g.SuiteDir))
	if m, c := g.Limits(); m == "" || c == 0 {
		report("WARN", "grader", "the grader's container runs with no memory or CPU limit (grader.memory: none or cpus: 0); a suite that hangs or leaks is held by the timeout only")
	}
}

// checkGraders finds the grader containers a killed ygg left, by their label, and removes the ones no
// live run holds: a container of the grader is removed by Grade itself, so one that is left means ygg
// died while it ran.
func (c *command) checkGraders(d *sandbox.Driver, project string, report func(level, what, msg string)) {
	left, err := d.GraderLeftovers(c.ctx)
	if err != nil {
		report("ERROR", "graders", err.Error())
		return
	}
	for _, l := range left {
		short := l.Container[:min(12, len(l.Container))]
		dir := run.Dir(project, l.Run)
		switch _, err := os.Stat(dir); {
		case !run.ValidID(l.Run) || err != nil:
			report("WARN", "graders", fmt.Sprintf("container %s of run %s belongs to another project; doctor in that project removes it", short, l.Run))
		case run.Locked(dir):
			// A live ygg is grading.
		default:
			if err := d.RemoveGrader(c.ctx, l.Container); err != nil {
				report("ERROR", "graders", fmt.Sprintf("couldn't remove %s of run %s: %v", short, l.Run, err))
			} else {
				report("WARN", "graders", fmt.Sprintf("run %s left the grader container %s (%s); removed it", l.Run, short, l.State))
			}
		}
	}
	report("OK", "graders", fmt.Sprintf("%d grader container(s) of ygg runs", len(left)))
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
