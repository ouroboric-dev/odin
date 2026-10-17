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
	present, err := seedScratch(r, n)
	if err != nil {
		return &nodeFailure{err}
	}
	prompt, err := os.ReadFile(filepath.Join(r.Setup.Realm(n.Realm).Dir, n.Prompt))
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
	text += "\n\n" + workspaceNote(r, present)
	if r.Setup.BriefText != "" {
		text += "\n\n## Brief\n\n" + r.Setup.BriefText
	}
	if reason := s.Reasons[n.ID]; reason != "" {
		text += "\n\n## Rejected at the approval point\n\n" + reason + "\n"
	}
	if refusal := s.Refusals[n.ID]; refusal != "" {
		text += "\n\n" + jormungandr.Refusal(refusal)
	}
	sp := sessionSpec{key: n.ID, role: n.Role, text: text, maxTurns: n.MaxTurns, timeout: time.Duration(n.Timeout),
		scratch: dir, reseed: func() error { _, err := seedScratch(r, n); return err }}
	res, ev, err := session(ctx, r, sp)
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
func workspaceNote(r *run.Run, present []string) string {
	var b strings.Builder
	b.WriteString("## Your workspace\n\nYou work in the folder you start in. It is not a repository, and nothing in it is committed by you.\n")
	if r.Setup.BriefText != "" {
		b.WriteString("- brief.md is the brief, as you are given it below.\n")
	}
	for _, p := range present {
		fmt.Fprintf(&b, "- %s is a copy of the project's file or folder of that name. Read it. What you change in it is ignored.\n", p)
	}
	b.WriteString("- Write every document you produce under out/, in the folders it should have in the project (for example out/docs/2-engineering/specs/...). " +
		"Only out/ leaves this workspace, and only after a person or a check has approved it.\n")
	return b.String()
}

// seedScratch makes the workspace of node n anew: the brief, a copy of each input that exists in the
// project, and an empty out/. It returns the inputs it copied. Odin's own folder is replaced as a whole;
// nothing outside it is touched. An input that is, or holds, a symlink or anything but a regular file is
// refused, since the workspace is a copy and must not lead out of the project.
func seedScratch(r *run.Run, n schema.Node) (present []string, err error) {
	dir := r.ScratchDir(n.ID)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		return nil, err
	}
	if r.Setup.BriefText != "" {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(r.Setup.BriefText), 0o644); err != nil {
			return nil, err
		}
	}
	b := &copyBudget{files: maxScratchFiles, bytes: maxScratchBytes}
	for _, in := range n.Inputs {
		src := filepath.Join(r.Project, filepath.FromSlash(in))
		if err := noSymlinks(r.Project, src); err != nil {
			return nil, fmt.Errorf("input %s of %s: %w; the workspace holds copies of files and nothing that could lead out of the project", in, n.ID, err)
		}
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			continue // not written yet, such as the leaves file before the spec stage
		}
		if err := copyTree(src, filepath.Join(dir, filepath.FromSlash(in)), b); err != nil {
			return nil, fmt.Errorf("input %s of %s: %w", in, n.ID, err)
		}
		present = append(present, in)
	}
	return present, nil
}

type copyBudget struct{ files, bytes int64 }

// copyTree copies the regular files under src to dst, leaving out .git. It stops at a symlink or any
// other kind of file, and at more files or bytes than the budget holds.
func copyTree(src, dst string, b *copyBudget) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
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
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if b.bytes -= int64(len(data)); b.bytes < 0 {
			return fmt.Errorf("more than %d bytes", maxScratchBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
