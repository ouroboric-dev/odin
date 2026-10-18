package routing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

// Verdict is what the pointer-contract gate finds (the ledger's pointer_contract_checked).
//   - Pass: the move changes no contract that a leaf reads. The gate opens by itself.
//   - Fail: the move changes a contract, and an extracted edge or oasdiff shows it. The approval point
//     applies: a person decides (spec §3.4).
//   - Escalate: the only evidence is a guess. A person decides in every mode.
type Verdict string

// Evidence is how the verdict was reached.
type Evidence string

const (
	Pass     Verdict = "pass"
	Fail     Verdict = "fail"
	Escalate Verdict = "escalate"

	Oasdiff Evidence = "oasdiff"
)

// Change is a file a pointer move changes. Path is from the project root. Old is nil for a file the move
// adds, New is nil for a file it deletes.
type Change struct {
	Path     string
	Old, New []byte
}

// Delta is what oasdiff finds between two versions of a contract.
type Delta struct{ Changed, Breaking bool }

// Differ compares two versions of a contract (oasdiff).
type Differ interface {
	Diff(ctx context.Context, old, new []byte) (Delta, error)
}

// Judgement is the verdict, the evidence behind it, and why.
type Judgement struct {
	Verdict   Verdict
	Evidence  Evidence
	Reason    string
	Contracts []string // the contract files whose change the verdict is about
}

// IsContractName reports whether a file's name says it holds an OpenAPI contract.
func IsContractName(path string) bool { return isContractName(path) }

var rank = map[Verdict]int{Pass: 0, Fail: 1, Escalate: 2}

// Judge decides whether a pointer move that makes changes is a pointer-contract move (spec §3.4, §8.2).
// It goes by extracted edges of the impact index, or by oasdiff, and never by a guess alone: a contract
// that only an inferred edge reaches, or that the index does not list, gives Escalate unless oasdiff
// shows the change. d may be nil when no oasdiff is at hand. Only files that are named like a contract
// matter; every move also runs the umbrella's acceptance tests, which catch the rest (spec §8.2).
func Judge(ctx context.Context, imp *Impact, changes []Change, d Differ) Judgement {
	if imp == nil {
		imp = &Impact{} // no index: every contract file is unlisted, so a guess at best
	}
	out := Judgement{Verdict: Pass, Evidence: Extracted, Reason: "no contract file changed"}
	first := true
	for _, c := range changes {
		if !isContractName(c.Path) {
			continue
		}
		j := judgeOne(ctx, imp, c, d)
		if j.Verdict != Pass {
			out.Contracts = append(out.Contracts, c.Path)
		}
		if first || rank[j.Verdict] > rank[out.Verdict] {
			out.Verdict, out.Evidence, out.Reason, first = j.Verdict, j.Evidence, j.Reason, false
		}
	}
	slices.Sort(out.Contracts)
	return out
}

func judgeOne(ctx context.Context, imp *Impact, c Change, d Differ) Judgement {
	var delta *Delta
	if d != nil && c.Old != nil && c.New != nil {
		if dl, err := d.Diff(ctx, c.Old, c.New); err == nil {
			delta = &dl
		}
	}
	if delta != nil && !delta.Changed {
		return Judgement{Pass, Oasdiff, c.Path + ": oasdiff finds no change in the contract", nil}
	}
	entry, known := imp.Contract(c.Path)
	if !known {
		if delta != nil {
			return Judgement{Fail, Oasdiff, fmt.Sprintf("%s: oasdiff finds a change (breaking: %t); the index does not list the file", c.Path, delta.Breaking), nil}
		}
		return Judgement{Escalate, Inferred, c.Path + ": looks like a contract by its name only; the index does not list it", nil}
	}
	ev := Evidence(Extracted)
	if delta != nil {
		ev = Oasdiff
	}
	switch {
	case len(entry.Readers.Extracted) > 0:
		return Judgement{Fail, ev, fmt.Sprintf("%s: changes a contract that %v read by extracted edges", c.Path, entry.Readers.Extracted), nil}
	case len(entry.Readers.Inferred) > 0:
		return Judgement{Escalate, Inferred, fmt.Sprintf("%s: only guesses link %v to this contract", c.Path, entry.Readers.Inferred), nil}
	}
	return Judgement{Pass, ev, c.Path + ": no leaf reads this contract (a lower bound)", nil}
}

// OasdiffCLI runs the oasdiff program: `oasdiff changelog old new` says whether anything changed, and
// `oasdiff breaking --fail-on ERR old new` exits 1 when a change breaks a client. The contracts are
// written to a new folder only Odin can read, and the program gets no environment but PATH.
type OasdiffCLI struct{ Bin string }

// HostOasdiff returns the oasdiff on the host's PATH, or nil when there is none.
func HostOasdiff() Differ {
	if p, err := exec.LookPath("oasdiff"); err == nil {
		return OasdiffCLI{Bin: p}
	}
	return nil
}

// Diff compares old with new.
func (o OasdiffCLI) Diff(ctx context.Context, old, new []byte) (Delta, error) {
	dir, err := os.MkdirTemp("", "ygg-oasdiff-")
	if err != nil {
		return Delta{}, err
	}
	defer os.RemoveAll(dir)
	for name, b := range map[string][]byte{"old.yaml": old, "new.yaml": new} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return Delta{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	run := func(args ...string) ([]byte, int, error) {
		cmd := exec.CommandContext(ctx, o.Bin, args...)
		cmd.Dir, cmd.Env = dir, []string{"PATH=" + os.Getenv("PATH")}
		var stdout bytes.Buffer
		cmd.Stdout = &limitWriter{w: &stdout, n: 1 << 20}
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), ee.ExitCode(), nil
		}
		return stdout.Bytes(), 0, err
	}
	out, code, err := run("changelog", "old.yaml", "new.yaml")
	if err != nil || code != 0 {
		return Delta{}, fmt.Errorf("oasdiff changelog: exit %d, %v", code, err)
	}
	d := Delta{Changed: len(bytes.TrimSpace(out)) > 0}
	if !d.Changed {
		return d, nil
	}
	_, code, err = run("breaking", "--fail-on", "ERR", "old.yaml", "new.yaml")
	switch {
	case err != nil:
		return Delta{}, fmt.Errorf("oasdiff breaking: %w", err)
	case code == 1:
		d.Breaking = true
	case code != 0:
		return Delta{}, fmt.Errorf("oasdiff breaking: exit %d", code)
	}
	return d, nil
}

// limitWriter keeps the first n bytes and drops the rest.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}
