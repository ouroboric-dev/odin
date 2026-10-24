package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// runScratch runs a scratch agent node (spec §3.3 rule 1): a planner gets a workspace of its own, not an
// umbrella and not a leaf. The workspace holds an empty out/ folder, where the agent writes what it produces, and
// the files of its inputs over the bound: what it reads is in its prompt, which promptFor builds from its declared
// inputs. Odin publishes out/ into the project later (a publish node), after the approval point. A workspace is
// made again for every attempt and every retry, so a half-written one never reaches the next.
func runScratch(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	dir := r.ScratchDir(n.ID)
	if err := seedScratch(r, n); err != nil {
		return &nodeFailure{err}
	}
	text, err := promptFor(r, n, nil, nil)
	if err != nil {
		return err
	}
	// A try that failed starts again from a fresh workspace, with the same input files.
	reseed := func() error {
		if err := seedScratch(r, n); err != nil {
			return err
		}
		_, err := promptFor(r, n, nil, nil)
		return err
	}
	// The session hands back one document of the node's contract, through the output channel: an output that
	// does not validate is asked for once more, with the errors, and a second one fails the node. A gate reads a
	// design-conformance verdict from there (spec §4.6).
	sp := sessionSpec{key: n.ID, role: n.Role, text: text, output: schema.Contract(n.Output), maxTurns: n.MaxTurns, maxTokens: n.MaxTokens,
		timeout: time.Duration(n.Timeout), scratch: dir, reseed: reseed}
	res, ev, err := sessionForOutput(ctx, r, sp)
	if err != nil {
		return nodeError(err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return err
	}
	delete(s.Reasons, n.ID)
	delete(s.Refusals, n.ID)
	s.Summaries[n.ID] = res.Summary
	return nil
}

// seedScratch makes the workspace of node n anew: an empty out/. Nothing of the project is copied in, not even
// the output of a node it reviews: a node's inputs are typed, and reach its prompt (change typed-boundaries); the
// ones over the bound are written by promptFor, after this. Everything in Odin's own folder is replaced; nothing
// outside it is touched. The folder itself stays, emptied in place: the scratch sandbox mounts it and stays up
// across a retry and a refusal sent back, and a folder made again would not be the one its container sees
// (review I-3).
func seedScratch(r *run.Run, n schema.Node) error {
	dir := r.ScratchDir(n.ID)
	if err := emptyDir(dir); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(dir, "out"), 0o755)
}

// emptyDir removes everything in dir and keeps dir, so a mount of it stays valid. A dir that is not
// there is made; anything at its path that is not a folder, such as a symlink, is removed first.
func emptyDir(dir string) error {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case !st.IsDir():
		if err := os.Remove(dir); err != nil {
			return err
		}
		return os.MkdirAll(dir, 0o755)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
