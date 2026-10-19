package deps

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/checks"
	"github.com/ouroboric-dev/gungnir"
)

// Ladder is the checks a bump runs in a product, in this order: the dependency policy, then the unit
// tests. Both are checks of the product's realm, run by the same check runner as a loop's ladder.
var Ladder = []string{"deps", "unit"}

// DefaultTimeout is how long one command of a bump may run.
const DefaultTimeout = 30 * time.Minute

// Product is one leaf a bump may change.
type Product struct {
	Name   string              // its path from the project, such as repos/api
	Dir    string              // its working copy, absolute
	Realm  string              // the realm that provides it
	Checks map[string][]string // that realm's checks: name, then the command
}

// Opener starts what runs a product's commands, and returns the function that stops it. In a project
// with a sandbox this is the product's sandbox: the tool and the checks run where an agent's would.
type Opener interface {
	Open(ctx context.Context, p Product) (gungnir.Runner, func(context.Context) error, error)
}

// Step is how the bump went in one product.
type Step struct {
	Product string
	Stage   string // canary or rollout
	From    string
	To      string
	Verdict string // pass, fail, or skip: the product has the version already
	Reason  string
}

// Report is what a bump did. A bump that failed a product is a Report with Failed true, not an error.
type Report struct{ Steps []Step }

// Failed reports whether a product failed its ladder, so the bump stopped.
func (r Report) Failed() bool {
	return len(r.Steps) > 0 && r.Steps[len(r.Steps)-1].Verdict == "fail"
}

// Refusal is a bump that was refused before it touched anything.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Bumper bumps a package.
type Bumper struct {
	Open    Opener
	Log     func(gungnir.Event) error // the ledger of the bump
	LogDir  string                    // where the output of each command is kept
	Timeout time.Duration             // zero is DefaultTimeout
	Guard   func(Product) error       // refuses a product that broke a sandbox rule (leaf.Guard); nil in tests
}

type plan struct {
	p     Product
	m     Manifest
	stage string
	skip  bool
}

// Bump moves pkg to version in the canary, runs the ladder there, and only if it passes does the same in
// each rollout product, one at a time. The first product that fails is put back as it was, and the bump
// stops: products after it are never touched. A policy file is never changed: the tool may change the
// manifest and its lock file, and nothing else, or the product fails. A refusal changes nothing.
func (b *Bumper) Bump(ctx context.Context, pkg, version string, canary Product, rollout []Product) (Report, error) {
	plans, to, err := b.plan(pkg, version, canary, rollout)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, pl := range plans {
		step := Step{Product: pl.p.Name, Stage: pl.stage, From: pl.m.Current.String(), To: to.String()}
		if pl.skip {
			step.Verdict, step.Reason = "skip", "it requires "+step.From+" already"
			rep.Steps = append(rep.Steps, step)
			continue
		}
		// A line with no verdict says the tool is about to run: if the command dies, the ledger shows where.
		if err := b.Log(gungnir.Event{Name: gungnir.EventDepsBump, Package: pkg, From: step.From, To: step.To,
			Stage: pl.stage, Note: cut(pl.p.Name+": started", 400)}); err != nil {
			return rep, err
		}
		var serr error
		step.Verdict, step.Reason, serr = b.stage(ctx, pkg, to, pl)
		if serr != nil && step.Verdict == "" {
			return rep, serr // a cancel or a sandbox error: the product is back, and no verdict is claimed
		}
		rep.Steps = append(rep.Steps, step)
		note := pl.p.Name
		if step.Reason != "" {
			note += ": " + step.Reason
		}
		if err := b.Log(gungnir.Event{Name: gungnir.EventDepsBump, Package: pkg, From: step.From, To: step.To,
			Stage: pl.stage, Verdict: step.Verdict, Note: cut(note, 400)}); err != nil {
			return rep, errors.Join(serr, err)
		}
		if serr != nil { // a product that could not be put back: the ledger says so, and the bump ends
			return rep, serr
		}
		if step.Verdict == "fail" {
			break
		}
	}
	return rep, nil
}

// plan checks everything that can be checked before a file changes.
func (b *Bumper) plan(pkg, version string, canary Product, rollout []Product) ([]plan, Version, error) {
	refuse := func(f string, a ...any) ([]plan, Version, error) {
		return nil, Version{}, &Refusal{fmt.Sprintf(f, a...)}
	}
	to, err := ParseVersion(version)
	switch {
	case !ValidPackage(pkg):
		return refuse("%q is not a package name", pkg)
	case err != nil:
		return refuse("%v", err)
	case to.Pre != "":
		return refuse("%s is a pre-release; the policy allows tagged releases only", version)
	}
	var plans []plan
	var dirs []string
	for i, p := range append([]Product{canary}, rollout...) {
		stage := "rollout"
		if i == 0 {
			stage = "canary"
		}
		if dir := filepath.Clean(p.Dir); slices.Contains(dirs, dir) {
			return refuse("the product %s is named twice", p.Name)
		} else {
			dirs = append(dirs, dir)
		}
		for _, name := range Ladder {
			if len(p.Checks[name]) == 0 {
				return refuse("the realm %s of %s defines no check %s; a bump never skips a rung", p.Realm, p.Name, name)
			}
		}
		m, err := Find(p.Dir, pkg)
		if err != nil {
			return refuse("%s: %v", p.Name, err)
		}
		if !ValidFor(m.Kind, pkg) {
			return refuse("%q is not a package name npm reads as a registry package: an unscoped name with a slash is a GitHub repository", pkg)
		}
		switch c := to.Compare(m.Current); {
		case c < 0:
			return refuse("%s requires %s %s: %s goes down, and a bump only goes up", p.Name, pkg, m.Current, to)
		case c == 0 && stage == "canary":
			return refuse("the canary %s requires %s %s already; there is nothing to bump", p.Name, pkg, m.Current)
		}
		plans = append(plans, plan{p: p, m: m, stage: stage, skip: to.Compare(m.Current) == 0})
	}
	return plans, to, nil
}

// stage bumps one product and runs its ladder. A product that fails is put back as it was; the error is
// for what is not the product's fault: the sandbox, the log, the snapshot or a context that ended.
func (b *Bumper) stage(ctx context.Context, pkg string, to Version, pl plan) (verdict, reason string, err error) {
	if b.Guard != nil { // a product that already breaks a rule is not bumped: the bump would hide it
		if err := b.Guard(pl.p); err != nil {
			return "", "", fmt.Errorf("%s breaks a sandbox rule before the bump: %w", pl.p.Name, err)
		}
	}
	snap, err := Take(pl.p.Dir)
	if err != nil {
		return "", "", err
	}
	end := context.WithoutCancel(ctx) // a restore and a stop must also happen after a cancel
	runner, stop, err := b.Open.Open(ctx, pl.p)
	if err != nil {
		return "", "", err
	}
	verdict, reason, err = b.apply(ctx, runner, snap, pkg, to, pl)
	if serr := stop(end); serr != nil && err == nil {
		err = serr
	}
	if err != nil && verdict == "pass" {
		verdict = "" // the sandbox did not stop cleanly: the bump claims no pass
	}
	if err != nil || verdict == "fail" {
		if rerr := snap.Restore(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("could not put %s back as it was: %w", pl.p.Name, rerr))
			// the ledger must say what is in the product
			if verdict == "fail" {
				reason += "; the product could not be put back"
			} else {
				verdict, reason = "fail", "the bump stopped ("+cut(err.Error(), 120)+") and the product could not be put back"
			}
		}
	}
	return verdict, reason, err
}

func (b *Bumper) apply(ctx context.Context, runner gungnir.Runner, snap *Snapshot, pkg string, to Version, pl plan) (verdict, reason string, err error) {
	run := func(name string, argv []string) (checks.Result, error) {
		t := b.Timeout
		if t == 0 {
			t = DefaultTimeout
		}
		log := filepath.Join(b.LogDir, strings.ReplaceAll(pl.p.Name, "/", "_")+"-"+pl.stage+"-"+name+".log")
		return checks.Run(ctx, runner, checks.Spec{Name: name, Argv: argv, Timeout: t}, log)
	}
	res, err := run("bump", pl.m.Command(pkg, to))
	if err != nil {
		return "", "", err
	}
	if !res.Passed {
		return "fail", "the tool failed: " + ended(res), nil
	}
	if reason, err := b.escaped(snap, pl); err != nil {
		return "", "", err
	} else if reason != "" {
		return "fail", "the tool " + reason, nil
	}
	if extra, err := outside(snap, pl); err != nil {
		return "", "", err
	} else if len(extra) > 0 {
		return "fail", "the tool changed files that are not the manifest or its lock file: " + strings.Join(extra, ", "), nil
	}
	if reason := wrongManifest(pkg, to, pl); reason != "" {
		return "fail", reason, nil
	}
	frozen, err := freeze(pl)
	if err != nil {
		return "", "", err
	}
	for _, name := range Ladder {
		res, err := run(name, pl.p.Checks[name])
		if err != nil {
			return "", "", err
		}
		if name == "deps" {
			v := "pass"
			if !res.Passed {
				v = "fail"
			}
			if err := b.Log(gungnir.Event{Name: gungnir.EventDepsChecked, Realm: pl.p.Realm, Verdict: v}); err != nil {
				return "", "", err
			}
		}
		if reason, err := b.escaped(snap, pl); err != nil {
			return "", "", err
		} else if reason != "" {
			return "fail", "the check " + name + " " + reason, nil
		}
		if !res.Passed {
			return "fail", "the check " + name + " failed: " + ended(res), nil
		}
	}
	// The rungs run the new dependency's code with the product writable: what they changed is judged too.
	if extra, err := outside(snap, pl); err != nil {
		return "", "", err
	} else if len(extra) > 0 {
		return "fail", "the checks changed files that are not the manifest or its lock file: " + strings.Join(extra, ", "), nil
	}
	if reason := wrongManifest(pkg, to, pl); reason != "" {
		return "fail", reason, nil
	}
	// The deps rung judged the lock before the unit rung ran: after the tool, the lock must not move.
	now, err := freeze(pl)
	if err != nil {
		return "", "", err
	}
	for _, f := range pl.m.Kind.Files() {
		if now[f] != frozen[f] {
			return "fail", "the checks changed " + f + " after the tool ran; the deps rung judged the old one", nil
		}
	}
	return "pass", "", nil
}

// outside lists the files that differ from the snapshot and are not the manifest's or its lock file.
func outside(snap *Snapshot, pl plan) ([]string, error) {
	changed, err := snap.Changes()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(changed, func(f string) bool { return slices.Contains(pl.m.Kind.Files(), f) }), nil
}

// wrongManifest says why the manifest is not what the bump planned: the new version, in the style it had.
func wrongManifest(pkg string, to Version, pl plan) string {
	m, err := Find(pl.p.Dir, pkg)
	switch {
	case err != nil || m.Current.Compare(to) != 0:
		return fmt.Sprintf("the manifest does not require %s %s after the tool ran", pkg, to)
	case m.Tilde != pl.m.Tilde:
		return fmt.Sprintf("the manifest's pin style for %s changed (a tilde was dropped or added); an .npmrc may override it", pkg)
	}
	return ""
}

func ended(r checks.Result) string {
	if r.TimedOut {
		return "it timed out"
	}
	return fmt.Sprintf("exit status %d (output: %s)", r.ExitCode, r.Log)
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// freeze reads the manifest and lock files the bump may change, as they are after the tool ran. A file that
// is not there, or that is a symlink, is a value too: its target, or a marker.
func freeze(pl plan) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range pl.m.Kind.Files() {
		p := filepath.Join(pl.p.Dir, f)
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out[f] = "\x00absent"
		case err != nil:
			return nil, err
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return nil, err
			}
			out[f] = "\x00link:" + l
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			out[f] = "file:" + string(b)
		}
	}
	return out, nil
}

// escaped says whether the product broke a sandbox rule: the top-level .git is not as it was, or the guard
// of the leaf (a rewritten .git file, a git repository below the top level, node_modules included) refuses.
func (b *Bumper) escaped(snap *Snapshot, pl plan) (string, error) {
	changed, err := snap.GitChanged()
	if err != nil {
		return "", err
	}
	if changed {
		return "changed the .git of the product", nil
	}
	if b.Guard != nil {
		if err := b.Guard(pl.p); err != nil {
			return "broke a sandbox rule: " + err.Error(), nil
		}
	}
	return "", nil
}
