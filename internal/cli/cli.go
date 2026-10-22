// Package cli is the ygg command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/budget"
	"github.com/ouroboric-dev/odin/internal/engine"
	"github.com/ouroboric-dev/odin/internal/run"
)

// Exit codes.
const (
	ExitDone    = 0 // the run finished, or the command succeeded
	ExitFailed  = 1 // a node failed; fix the cause, then resume
	ExitRefused = 2 // bad usage or config: nothing ran
	ExitStopped = 3 // the run waits at an approval point
	ExitPaused  = 4 // the run waits for the provider's quota; resume it once the quota is back
	ExitBlocked = 5 // an unattended run finished, and gave up tasks whose loops gave up: they are listed first
)

const usage = `usage:
  ygg run     --graph <name|dir> [--project <dir>] [--config <factory.yaml>] [--brief <file>] [--mode <mode>]
  ygg resume  <run-id> [--project <dir>] [--mode <mode>]
  ygg approve <run-id> <gate> [--note <text>] [--project <dir>] [--mode <mode>]
  ygg reject  <run-id> <gate> --reason <text> [--project <dir>] [--mode <mode>]
  ygg status  <run-id> [--project <dir>]
  ygg grade   <run-id> [--task <id>] [--project <dir>]
  ygg deps bump <package> <version> --canary <leaf> [--rollout <leaf>]... [--host] [--project <dir>] [--config <factory.yaml>]
  ygg doctor  [--project <dir>] [--config <factory.yaml>] [--root <dir>]
  ygg sandbox build (--realm <name|dir>)... | --all [--config <factory.yaml>]
exit codes: 0 done, 1 a node failed, 2 refused, 3 stopped at an approval point, 4 paused: the provider's quota is used up,
  5 done, with tasks given up (unattended)
`

// Main runs one ygg command and returns its exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitRefused
	}
	c := &command{ctx: ctx, out: stdout, err: stderr}
	cmds := map[string]func([]string) int{
		"run": c.run, "resume": c.resume, "approve": c.approve, "reject": c.reject,
		"status": c.status, "doctor": c.doctor, "sandbox": c.sandbox, "grade": c.grade, "deps": c.deps,
	}
	f, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "ygg: unknown command %q\n%s", args[0], usage)
		return ExitRefused
	}
	return f(args[1:])
}

type command struct {
	ctx      context.Context
	out, err io.Writer
}

// flags holds every flag; each command reads the ones it defines.
type flags struct {
	project, config, graph, realm, brief, mode, note, reason, root, task, canary string
	realms, rollout                                                              []string        // sandbox build: every --realm given; deps bump: every --rollout given
	all, host                                                                    bool            // sandbox build: --all; deps bump: --host
	set                                                                          map[string]bool // the flags given
}

// parse parses flags that may come before, between or after the positional arguments, and
// checks that there are exactly want of those.
func (c *command) parse(name string, args []string, want int, define func(*flag.FlagSet, *flags)) (*flags, []string, bool) {
	fs := flag.NewFlagSet("ygg "+name, flag.ContinueOnError)
	fs.SetOutput(c.err)
	f := &flags{}
	fs.StringVar(&f.project, "project", ".", "the project directory")
	fs.StringVar(&f.mode, "mode", "", "step-by-step, key-points or unattended")
	define(fs, f)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, false
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) != want {
		fmt.Fprintf(c.err, "ygg %s: want %d argument(s), got %d\n%s", name, want, len(pos), usage)
		return nil, nil, false
	}
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	return f, pos, true
}

func (c *command) mode(f *flags) (gungnir.Mode, bool) {
	if f.mode == "" {
		return "", true
	}
	m, err := gungnir.ParseMode(f.mode)
	if err != nil {
		fmt.Fprintf(c.err, "ygg: %v\n", err)
		return "", false
	}
	return m, true
}

func (c *command) refuse(err error) int {
	fmt.Fprintf(c.err, "ygg: %v\n", err)
	return ExitRefused
}

func (c *command) run(args []string) int {
	f, _, ok := c.parse("run", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
		fs.StringVar(&f.graph, "graph", "", "the graph pack: a name found in factory.yaml's graphs.dir, or the directory that holds pack.yaml")
		fs.StringVar(&f.realm, "realm", "", "no longer a flag of a run: a leaf's realm is pinned in the project (use --graph)")
		fs.StringVar(&f.brief, "brief", "", "the brief")
	})
	if !ok {
		return ExitRefused
	}
	if f.set["realm"] {
		return c.refuse(errors.New("use --graph; a leaf's realm is pinned in the project"))
	}
	if f.graph == "" {
		return c.refuse(errors.New("run needs --graph"))
	}
	mode, ok := c.mode(f)
	if !ok {
		return ExitRefused
	}
	r, err := run.Start(c.ctx, run.Options{Project: f.project, Config: f.config, Graph: f.graph, Brief: f.brief, Mode: mode})
	var failed *run.Failed
	if errors.As(err, &failed) { // it started, so run.json records it: fix the cause, then resume
		fmt.Fprintf(c.err, "Run %s failed: %v\n%s", failed.ID, failed.Err, resumeHint(failed.ID))
		return ExitFailed
	}
	if err != nil {
		return c.refuse(err)
	}
	defer r.Close()
	fmt.Fprintf(c.out, "Run %s started in %s mode.\n", r.State.ID, r.State.Mode)
	return c.advance(r)
}

// resumeHint tells the operator how to go on after a failure, and what a resume does to the leaves.
func resumeHint(id string) string {
	return fmt.Sprintf("Fix the cause, then run: ygg resume %s\n"+
		"A resume puts each leaf back to its last checkpoint; changes made since are saved under refs/ygg/%s/discarded/.\n", id, id)
}

// open opens a run for resume, approve and reject, and says where Open saved what it discarded.
func (c *command) open(f *flags, id string) (*run.Run, int) {
	mode, ok := c.mode(f)
	if !ok {
		return nil, ExitRefused
	}
	r, err := run.Open(c.ctx, f.project, id, mode)
	if err != nil {
		return nil, c.refuse(err)
	}
	for _, s := range r.Saved {
		fmt.Fprintf(c.err, "ygg: %s\n", s)
	}
	return r, ExitDone
}

func (c *command) resume(args []string) int {
	f, pos, ok := c.parse("resume", args, 1, func(*flag.FlagSet, *flags) {})
	if !ok {
		return ExitRefused
	}
	r, code := c.open(f, pos[0])
	if r == nil {
		return code
	}
	defer r.Close()
	switch r.State.Status {
	case run.StatusDone:
		return c.refuse(fmt.Errorf("run %s is done; there is nothing to resume", r.State.ID))
	case run.StatusStopped:
		// Save keeps a --mode switch made at this pause; advance then reports the gate it waits at.
		if err := r.Save(); err != nil {
			return c.refuse(err)
		}
		return c.advance(r)
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunResumed, Mode: r.State.Mode, Note: strings.Join(r.Saved, "; ")}); err != nil {
		return c.refuse(err)
	}
	return c.advance(r)
}

func (c *command) decide(name string, args []string) int {
	f, pos, ok := c.parse(name, args, 2, func(fs *flag.FlagSet, f *flags) {
		if name == "approve" {
			fs.StringVar(&f.note, "note", "", "a note on the approval")
		} else {
			fs.StringVar(&f.reason, "reason", "", "why the work goes back")
		}
	})
	if !ok {
		return ExitRefused
	}
	r, code := c.open(f, pos[0])
	if r == nil {
		return code
	}
	defer r.Close()
	var err error
	if name == "approve" {
		err = engine.Approve(c.ctx, r, pos[1], f.note)
	} else {
		err = engine.Reject(c.ctx, r, pos[1], f.reason)
	}
	if err != nil {
		return c.refuse(err)
	}
	return c.advance(r)
}

func (c *command) approve(args []string) int { return c.decide("approve", args) }
func (c *command) reject(args []string) int  { return c.decide("reject", args) }

// advance walks the run and reports where it stopped.
func (c *command) advance(r *run.Run) int {
	out, err := engine.Advance(c.ctx, r)
	if err != nil {
		fmt.Fprintf(c.err, "ygg: run %s: %v\n", r.State.ID, err)
		return ExitFailed
	}
	id := r.State.ID
	for _, w := range out.Warn {
		fmt.Fprintf(c.err, "ygg: warning: %s\nygg: ygg doctor finds and stops what is left.\n", w)
	}
	switch out.Status {
	case run.StatusDone:
		if blocked := blockedTasks(r.State); len(blocked) > 0 {
			fmt.Fprintf(c.out, "Run %s is done, with %d task(s) given up. They are listed first, because they need you:\n%s", id, len(blocked), strings.Join(blocked, ""))
			return ExitBlocked
		}
		fmt.Fprintf(c.out, "Run %s is done.\n", id)
		return ExitDone
	case run.StatusStopped:
		approve, reject := engine.Approvable(r), engine.Rejectable(r)
		gates := filepath.Join(r.Dir, "gates")
		switch {
		case reject && approve:
			fmt.Fprintf(c.out, "Run %s stopped at approval point %s. Read %s, then run one of:\n"+
				"  ygg approve %s %s [--note \"...\"]\n  ygg reject %s %s --reason \"...\"\n",
				id, out.Gate, gates, id, out.Gate, id, out.Gate)
		case reject:
			fmt.Fprintf(c.out, "Run %s stopped at approval point %s, which takes no approval. Read %s, then run:\n"+
				"  ygg reject %s %s --reason \"...\"\n", id, out.Gate, gates, id, out.Gate)
		default:
			fmt.Fprintf(c.out, "Run %s stopped at the hard approval point %s. Read %s, then run:\n"+
				"  ygg approve %s %s [--note \"...\"]\nA hard stop takes no rejection: to drop the work, start a new run.\n",
				id, r.State.PendingGate, gates, id, out.Gate)
		}
		return ExitStopped
	case run.StatusPaused:
		fmt.Fprintf(c.err, "Run %s paused: %v\nNothing is lost. Once the provider's quota is back, run: ygg resume %s\n"+
			"A resume starts the node again from its last checkpoint.\n", id, out.Err, id)
		return ExitPaused
	default:
		fmt.Fprintf(c.err, "Run %s failed: %v\n%s", id, out.Err, resumeHint(id))
		return ExitFailed
	}
}

func (c *command) status(args []string) int {
	f, pos, ok := c.parse("status", args, 1, func(*flag.FlagSet, *flags) {})
	if !ok {
		return ExitRefused
	}
	if !run.ValidID(pos[0]) {
		return c.refuse(fmt.Errorf("%q is not a run id", pos[0]))
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	dir := run.Dir(project, pos[0])
	s, err := run.LoadState(dir)
	if err != nil {
		return c.refuse(err)
	}
	status := string(s.Status)
	if s.Status == run.StatusRunning && !run.Locked(dir) {
		status += " (no ygg process holds it: interrupted; ygg resume " + s.ID + ")"
	}
	fmt.Fprintf(c.out, "run:      %s\nstatus:   %s\nmode:     %s\ngraph:    %s %s\nposition: %d\n",
		s.ID, status, s.Mode, s.Graph, s.GraphVersion, s.Position)
	if s.Pending != "" {
		fmt.Fprintf(c.out, "waiting:  %s (%s)\n", s.Pending, s.PendingGate)
	}
	for _, line := range blockedTasks(s) {
		fmt.Fprintf(c.out, "blocked:  %s", line)
	}
	for _, p := range slices.Sorted(maps.Keys(s.Leaves)) {
		l := s.Leaves[p]
		from := ""
		if l.Realm != "" {
			from = fmt.Sprintf(" (%s %s)", l.Realm, l.RealmVersion)
		}
		fmt.Fprintf(c.out, "leaf:     %s on %s at %.12s%s\n", p, l.Branch, l.Checkpoint, from)
	}
	if len(s.Features) > 0 {
		counts := map[schema.TaskStatus]int{}
		for _, f := range s.Features {
			counts[f.Status]++
		}
		fmt.Fprintf(c.out, "tasks:    %d (%d done, %d doing, %d to do, %d blocked)\n", len(s.Features),
			counts[schema.TaskDone], counts[schema.TaskDoing], counts[schema.TaskTodo], counts[schema.TaskBlocked])
	}
	for _, id := range slices.Sorted(maps.Keys(s.Loops)) {
		l := s.Loops[id]
		line := fmt.Sprintf("attempt %d of %d, %s, %d finding(s) open", min(l.State.Attempt, l.State.Limit), l.State.Limit, l.State.Phase, len(l.State.Book.Open()))
		if l.State.Passed {
			line = fmt.Sprintf("passed in attempt %d, %d finding(s) open", l.State.Attempt, len(l.State.Book.Open()))
		}
		if l.Escalation != nil {
			line = fmt.Sprintf("gave up in attempt %d (%s), %d finding(s) open", min(l.State.Attempt, l.State.Limit), l.Escalation.Reason, len(l.State.Book.Open()))
		}
		fmt.Fprintf(c.out, "loop:     %s: %s\n", id, line)
		for _, o := range engine.Overruled(&l.State) {
			// A person audits these: the reviewer's claim, then the implementer's evidence and the check run.
			fmt.Fprintf(c.out, "overruled: %s: %s (%s, from %s) at %s\n  reviewer's claim: %s\n  %s\n", id, o.ID, o.Severity, o.Source, o.Location,
				strings.TrimSpace(o.Claim), strings.ReplaceAll(strings.TrimSpace(o.Proof), "\n", "\n  "))
		}
	}
	if s.Paused != nil {
		fmt.Fprintf(c.out, "paused:   at %s since %s: %s\n", s.Paused.Node, s.Paused.Since.Format(time.RFC3339), s.Paused.Reason)
	}
	if s.Error != "" {
		fmt.Fprintf(c.out, "error:    %s\n", s.Error)
	}
	u := s.Usage
	fmt.Fprintf(c.out, "usage:    %d tokens in, %d out, %d from cache; %d turns; $%.4f\n",
		u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	if line := capsLine(s); line != "" {
		fmt.Fprintf(c.out, "caps:     %s\n", line)
	}
	c.bumpLines(project, s.ConfigPath)
	return ExitDone
}

// blockedTasks lists the tasks the run gave up, one line each, with why.
func blockedTasks(s *run.State) []string {
	var out []string
	for _, f := range s.Features {
		if f.Status == schema.TaskBlocked {
			out = append(out, fmt.Sprintf("%s: %s\n", f.ID, f.Why))
		}
	}
	return out
}

// capsLine says how much of the caps the run and the day have used, from the run's config as it is now.
// It is empty when the config can't be read: status then says nothing about caps, rather than guess.
func capsLine(s *run.State) string {
	cfg, err := schema.LoadFactory(s.ConfigPath)
	if err != nil {
		return ""
	}
	j, err := run.BudgetJournal(cfg, nil)
	if err != nil {
		return ""
	}
	m := &budget.Meter{Run: &s.Usage, Caps: cfg.Budget, USD: cfg.PaysPerToken(), Journal: j}
	day, err := m.Today()
	if err != nil {
		return "the day's spend can't be read: " + err.Error()
	}
	return describeCaps(cfg, s.Usage, day)
}

// describeCaps is the run's and the day's use of each cap, in words.
func describeCaps(cfg *schema.Factory, runUse, dayUse gungnir.Usage) string {
	one := func(c schema.Cap, u gungnir.Usage) string {
		out := fmt.Sprintf("%d of %d tokens, %d of %d turns", u.CapTokens(), c.Tokens, u.Turns, c.Turns)
		if cfg.PaysPerToken() {
			out += fmt.Sprintf(", $%.4f billed of $%.4f", u.BilledUSD, c.USD)
		}
		return out
	}
	return "this run " + one(cfg.Budget.PerRun, runUse) + "; today " + one(cfg.Budget.PerDay, dayUse)
}

// bumpLines says what a killed bump left in the project: an unfinished ledger, and its containers.
func (c *command) bumpLines(project, configPath string) {
	ids, msgs, err := c.unfinishedBumps(project, false, "A container of its sandbox may still hold the product, so git is not safe there yet. Run: ygg doctor --project "+project)
	for i := range ids {
		fmt.Fprintf(c.out, "bump:     %s: %s\n", ids[i], msgs[i])
	}
	if err != nil {
		fmt.Fprintf(c.out, "bump:     %v\n", err)
	}
	cfg, _ := schema.LoadFactory(configPath)
	for _, l := range c.bumpContainers(project, cfg) {
		fmt.Fprintf(c.out, "bump:     %s: a container of the sandbox of %s is left (%s). Run: ygg doctor --project %s\n", l.Run, l.Leaf, l.State, project)
	}
}
