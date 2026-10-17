package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gleipnir/scan"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// What a scratch agent may publish: documents, a few hundred of them. Anything else in out/ is refused,
// not skipped, so a planner that wrote a script finds out why it was not published.
const (
	maxPublishFiles = 500
	maxPublishFile  = 1 << 20
	maxPublishBytes = 8 << 20
)

var (
	publishExt  = []string{".md", ".yaml", ".yml", ".json", ".txt", ".toml", ".d2", ".mmd"}
	publishName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// runPublish copies the documents a scratch agent wrote under out/ into the project, under the folder
// the node names, and commits them in the project umbrella (spec §3.3 rule 1: "Odin commits the
// planner's output into L1 after the spec approval point"). The files are checked first: only regular
// files with the names and endings of documents, in bounded number and size, and with no secret in
// them when the run has a sandbox to scan in. Odin commits with host git, by explicit paths.
func runPublish(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	out := filepath.Join(r.ScratchDir(n.From), "out")
	files, err := listDocuments(out)
	if err != nil {
		return &nodeFailure{fmt.Errorf("%s publishes what %s wrote, and %w", n.ID, n.From, err)}
	}
	if err := scanDocuments(ctx, r, n, files); err != nil {
		return err
	}
	project := r.Project
	if staged, err := git.Run(ctx, project, "diff", "--cached", "--name-only"); err != nil {
		return err
	} else if staged != "" {
		return &nodeFailure{fmt.Errorf("the project has staged changes Odin didn't make (%s); commit or unstage them first", strings.ReplaceAll(staged, "\n", ", "))}
	}
	var paths []string
	for _, rel := range files {
		dst := path.Join(n.To, rel)
		if err := copyDocument(project, filepath.Join(out, filepath.FromSlash(rel)), filepath.Join(project, filepath.FromSlash(dst))); err != nil {
			return &nodeFailure{fmt.Errorf("publishing %s: %w", dst, err)}
		}
		paths = append(paths, dst)
	}
	ev := gungnir.Event{Name: gungnir.EventDocsPublished, Node: n.ID, Attempt: attempt}
	if len(paths) > 0 {
		if _, err := git.Run(ctx, project, append([]string{"add", "--"}, paths...)...); err != nil {
			return err
		}
		if changed, err := git.Run(ctx, project, "diff", "--cached", "--name-only"); err != nil {
			return err
		} else if changed != "" {
			msg := fmt.Sprintf("Publish %s's documents (run %s, node %s)", n.From, r.State.ID, n.ID)
			if _, err := git.Run(ctx, project, append([]string{"commit", "-q", "-m", msg, "--"}, paths...)...); err != nil {
				return err
			}
			if ev.Commit, err = git.Run(ctx, project, "rev-parse", "HEAD"); err != nil {
				return err
			}
		}
	}
	switch {
	case len(paths) == 0:
		ev.Note = n.From + " wrote nothing under out/"
	case ev.Commit == "":
		ev.Note = fmt.Sprintf("%d file(s) under %s, unchanged", len(paths), n.To)
	default:
		ev.Note = fmt.Sprintf("%d file(s) under %s: %s", len(paths), n.To, strings.Join(paths[:min(len(paths), 5)], ", "))
	}
	r.State.Summaries[n.ID] = ev.Note
	return r.Log(ev)
}

// listDocuments lists the files under out as slash paths, sorted, and refuses what a document is not: a
// symlink, a special file, a hidden name, an odd character, an ending that is not a document's, a file
// over the size limit, or more files or bytes than the limits.
func listDocuments(out string) ([]string, error) {
	if st, err := os.Lstat(out); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("its output folder %s is not there (the node has not run)", out)
	}
	var files []string
	var total int64
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(out, p)
		if err != nil || rel == "." {
			return err
		}
		for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
			if !publishName.MatchString(seg) {
				return fmt.Errorf("%s has the name %q; a document's folders and files are named with letters, digits, dots, dashes and underscores, and do not start with a dot", rel, seg)
			}
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is %s, not a regular file", rel, d.Type())
		}
		if !slices.Contains(publishExt, strings.ToLower(filepath.Ext(rel))) {
			return fmt.Errorf("%s does not end like a document (%s)", rel, strings.Join(publishExt, ", "))
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		switch {
		case info.Size() > maxPublishFile:
			return fmt.Errorf("%s is %d bytes; a document is at most %d", rel, info.Size(), maxPublishFile)
		case len(files) >= maxPublishFiles:
			return fmt.Errorf("it wrote more than %d files", maxPublishFiles)
		case total > maxPublishBytes:
			return fmt.Errorf("it wrote more than %d bytes", maxPublishBytes)
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(files)
	return files, err
}

// scanDocuments looks for secrets in the documents in the scratch sandbox of the node that wrote them,
// where gitleaks is pinned (spec §14). A run with no sandbox has nowhere to run the scanner, and only
// scripted agents. Secrets are a hard stop at real-secrets-or-user-data with nothing published; a scan
// that could not run fails the node, because no answer is never "clean".
func scanDocuments(ctx context.Context, r *run.Run, n schema.Node, files []string) error {
	runner, err := r.ScratchRunner(ctx, n.From)
	if err != nil {
		return sandboxStart(err)
	}
	if runner == nil || len(files) == 0 {
		return nil
	}
	listed := make([]string, len(files))
	for i, f := range files {
		listed[i] = path.Join("out", f)
	}
	err = scan.Run(ctx, runner, listed)
	var found *scan.FoundError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &found):
		return &hardStop{gate: gungnir.GateRealSecrets, err: err}
	}
	return &nodeFailure{err}
}

// noSymlinks refuses a path under the project if any part of it that exists, from the project down, is a
// symlink: a write or a read through one would leave the project. Parts that don't exist yet are fine.
func noSymlinks(project, path string) error {
	rel, err := filepath.Rel(project, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s is not inside the project", path)
	}
	cur := project
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", cur)
		}
	}
	return nil
}

// copyDocument writes src to dst, making dst's folders. Nothing on the way to dst may be a symlink: a
// document must land where the node says, inside the project.
func copyDocument(project, src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := noSymlinks(project, dst); err != nil {
		return err
	}
	if st, err := os.Lstat(dst); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
