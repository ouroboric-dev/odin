package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// What a scratch workspace may be seeded with: a plan's documents are a few hundred small text files.
const (
	maxScratchFiles = 2000
	maxScratchBytes = 16 << 20
)

// runScratch runs a scratch agent node (spec §3.3 rule 1): a planner gets a workspace of its own, not an
// umbrella and not a leaf. The workspace holds the brief, read-only copies of the project paths the node
// names, and an empty out/ folder where the agent writes what it produces. Odin publishes out/ into the
// project later (a publish node), after the approval point. A workspace is made again for every attempt
// and every retry, so a half-written one never reaches the next.
func runScratch(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	dir := r.ScratchDir(n.ID)
	if err := seedScratch(r, n); err != nil {
		return &nodeFailure{err}
	}
	prompt, err := readPrompt(r.Setup, n.Realm, n.Prompt)
	if err != nil {
		return err
	}
	text := string(prompt)
	skillText, err := r.Setup.Libs.Load(n.Skills)
	if err != nil {
		return err
	}
	if skillText != "" {
		text += "\n\n" + skillText
	}
	text += "\n\n" + workspaceNote(r, n.Reviews)
	if r.Setup.BriefText != "" {
		text += "\n\n## Brief\n\n" + r.Setup.BriefText
	}
	text = withDecisions(text, s)
	if reason := s.Reasons[n.ID]; reason != "" {
		text += "\n\n## Rejected at the approval point\n\n" + reason + "\n"
	}
	if refusal := s.Refusals[n.ID]; refusal != "" {
		text += "\n\n" + jormungandr.Refusal(refusal)
	}
	// The session hands back one document of the node's contract, through the output channel: an output that
	// does not validate is asked for once more, with the errors, and a second one fails the node. A gate reads a
	// design-conformance verdict from there (spec §4.6).
	sp := sessionSpec{key: n.ID, role: n.Role, text: text, output: schema.Contract(n.Output), maxTurns: n.MaxTurns, maxTokens: n.MaxTokens,
		timeout: time.Duration(n.Timeout), scratch: dir, reseed: func() error { return seedScratch(r, n) }}
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

// workspaceNote tells the agent what its workspace holds and where its output goes.
func workspaceNote(r *run.Run, reviews string) string {
	var b strings.Builder
	b.WriteString("## Your workspace\n\nYou work in the folder you start in. It is not a repository, and nothing in it is committed by you.\n")
	if r.Setup.BriefText != "" {
		b.WriteString("- brief.md is the brief, as you are given it below.\n")
	}
	if reviews != "" {
		fmt.Fprintf(&b, "- reviewed/ holds the output of the node %s, which you review: what it wrote under its out/. Read it. What you change in it is ignored.\n", reviews)
	}
	b.WriteString("- Write every document you produce under out/, in the folders it should have in the project (for example out/docs/2-engineering/specs/...). " +
		"Only out/ leaves this workspace, and only after a person or a check has approved it.\n")
	return b.String()
}

// seedScratch makes the workspace of node n anew: the brief and an empty out/. A node that reviews another
// scratch agent also gets a copy of that agent's out/ as reviewed/, and a file there that is, or holds, a
// symlink or anything but a regular file is refused. No project folder is copied in: a node's inputs are typed,
// and reach its prompt (typed-T16). Everything in Odin's own folder is replaced; nothing outside it is touched.
// The folder itself stays, emptied in place: the scratch sandbox mounts it and stays up across a retry and a
// refusal sent back, and a folder made again would not be the one its container sees (review I-3).
func seedScratch(r *run.Run, n schema.Node) error {
	dir := r.ScratchDir(n.ID)
	if err := emptyDir(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		return err
	}
	if r.Setup.BriefText != "" {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(r.Setup.BriefText), 0o644); err != nil {
			return err
		}
	}
	b := &copyBudget{files: maxScratchFiles, bytes: maxScratchBytes}
	if n.Reviews != "" {
		// The output of the agent under review, as a copy: the reviewer reads it and cannot change it.
		src := filepath.Join(r.ScratchDir(n.Reviews), "out")
		if st, err := os.Lstat(src); err != nil || !st.IsDir() {
			return fmt.Errorf("node %s reviews %s, which has no output to review", n.ID, n.Reviews)
		}
		if err := copyTree(src, src, filepath.Join(dir, "reviewed"), b); err != nil {
			return fmt.Errorf("the output of %s, which %s reviews: %w", n.Reviews, n.ID, err)
		}
	}
	return nil
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

type copyBudget struct{ files, bytes int64 }

// copyTree copies the regular files under src, a path in project, to dst, leaving out .git and the
// project's .factory. It stops at a symlink or any other kind of file, and at more files or bytes than the
// budget holds. Each file is read as the regular file of the project the walk found, through what is left
// of the budget (readRegular).
func copyTree(project, src, dst string, b *copyBudget) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(project, p)
		if err != nil {
			return err
		}
		// .git anywhere, and the project's .factory even when an input holds the project's root (review M-4).
		if d.Name() == ".git" || name == ".factory" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is %s, not a regular file; the workspace holds copies of files and nothing that could lead out of the project", p, d.Type())
		}
		if b.files--; b.files < 0 {
			return fmt.Errorf("more than %d files", maxScratchFiles)
		}
		listed, err := d.Info()
		if err != nil {
			return err
		}
		data, err := readRegular(project, name, listed, b.bytes)
		var big *tooBigError
		switch {
		case errors.As(err, &big):
			return fmt.Errorf("more than %d bytes", maxScratchBytes)
		case err != nil:
			return fmt.Errorf("%w; the workspace holds copies of files and nothing that could lead out of the project", err)
		}
		b.bytes -= int64(len(data))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
