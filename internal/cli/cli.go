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

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/engine"
	"github.com/ouroboric-dev/odin/internal/run"
)

// Exit codes.
const (
	ExitDone    = 0 // the run finished, or the command succeeded
	ExitFailed  = 1 // a node failed; fix the cause, then resume
	ExitRefused = 2 // bad usage or config: nothing ran
	ExitStopped = 3 // the run waits at an approval point
)

const usage = `usage:
  ygg run     --realm <dir> [--project <dir>] [--config <factory.yaml>] [--brief <file>] [--mode <mode>]
  ygg resume  <run-id> [--project <dir>] [--mode <mode>]
  ygg approve <run-id> <gate> [--note <text>] [--project <dir>] [--mode <mode>]
  ygg reject  <run-id> <gate> --reason <text> [--project <dir>] [--mode <mode>]
  ygg status  <run-id> [--project <dir>]
  ygg doctor  [--project <dir>] [--config <factory.yaml>] [--root <dir>]
exit codes: 0 done, 1 a node failed, 2 refused, 3 stopped at an approval point
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
		"status": c.status, "doctor": c.doctor,
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
	project, config, realm, brief, mode, note, reason, root string
	set                                                     map[string]bool // the flags given
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
		fs.StringVar(&f.realm, "realm", "", "the realm directory (holds pack.yaml)")
		fs.StringVar(&f.brief, "brief", "", "the brief")
	})
	if !ok {
		return ExitRefused
	}
	if f.realm == "" {
		return c.refuse(errors.New("run needs --realm"))
	}
	mode, ok := c.mode(f)
	if !ok {
		return ExitRefused
	}
	r, err := run.Start(c.ctx, run.Options{Project: f.project, Config: f.config, Realm: f.realm, Brief: f.brief, Mode: mode})
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
	switch out.Status {
	case run.StatusDone:
		fmt.Fprintf(c.out, "Run %s is done.\n", id)
		return ExitDone
	case run.StatusStopped:
		fmt.Fprintf(c.out, "Run %s stopped at approval point %s. Read %s, then run one of:\n"+
			"  ygg approve %s %s [--note \"...\"]\n  ygg reject %s %s --reason \"...\"\n",
			id, out.Gate, filepath.Join(r.Dir, "gates"), id, out.Gate, id, out.Gate)
		return ExitStopped
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
	fmt.Fprintf(c.out, "run:      %s\nstatus:   %s\nmode:     %s\nrealm:    %s %s (graph %s)\nposition: %d\n",
		s.ID, status, s.Mode, s.Realm, s.RealmVersion, s.GraphVersion, s.Position)
	if s.Pending != "" {
		fmt.Fprintf(c.out, "waiting:  %s (%s)\n", s.Pending, s.PendingGate)
	}
	for _, p := range slices.Sorted(maps.Keys(s.Leaves)) {
		fmt.Fprintf(c.out, "leaf:     %s on %s at %.12s\n", p, s.Leaves[p].Branch, s.Leaves[p].Checkpoint)
	}
	if s.Error != "" {
		fmt.Fprintf(c.out, "error:    %s\n", s.Error)
	}
	u := s.Usage
	fmt.Fprintf(c.out, "usage:    %d tokens in, %d out, %d from cache; %d turns; $%.4f\n",
		u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	return ExitDone
}
