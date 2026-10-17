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
	"syscall"

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
	docs, err := readDocuments(out, files)
	if err != nil {
		return &nodeFailure{fmt.Errorf("%s publishes what %s wrote, and %w", n.ID, n.From, err)}
	}
	if err := scanDocuments(ctx, r, n, files, docs); err != nil {
		return err
	}
	project := r.Project
	if staged, err := git.Run(ctx, project, "diff", "--cached", "--name-only"); err != nil {
		return err
	} else if staged != "" {
		return &nodeFailure{fmt.Errorf("the project has staged changes Odin didn't make (%s); commit or unstage them first", strings.ReplaceAll(staged, "\n", ", "))}
	}
	var paths []string
	for i, rel := range files {
		dst := path.Join(n.To, rel)
		if err := copyDocument(project, docs[i], filepath.Join(project, filepath.FromSlash(dst))); err != nil {
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

// readDocuments reads each listed document once, as the regular file the listing found, within the limits:
// these bytes are the ones scanned and published, whatever happens in out/ after.
func readDocuments(out string, files []string) ([][]byte, error) {
	docs := make([][]byte, len(files))
	var total int64
	for i, rel := range files {
		b, err := readRegular(out, filepath.FromSlash(rel), maxPublishFile)
		if err != nil {
			return nil, err
		}
		if total += int64(len(b)); total > maxPublishBytes {
			return nil, fmt.Errorf("it wrote more than %d bytes", maxPublishBytes)
		}
		docs[i] = b
	}
	return docs, nil
}

// publishStage is the folder of the scratch workspace where Odin writes the documents it read, for the
// scan: out/ as Odin read it, not as it is when the scan runs.
const publishStage = ".publish"

// scanDocuments looks for secrets in the documents in the scratch sandbox of the node that wrote them,
// where gitleaks is pinned (spec §14). A run with no sandbox has nowhere to run the scanner, and only
// scripted agents. Secrets are a hard stop at real-secrets-or-user-data with nothing published; a scan
// that could not run fails the node, because no answer is never "clean". The scan reads docs, the bytes
// Odin read and will publish: Odin writes them under publishStage in the workspace, at their paths in
// out/, and runs the scan there, so its report names them as out/ does.
func scanDocuments(ctx context.Context, r *run.Run, n schema.Node, files []string, docs [][]byte) error {
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
	if err := stageDocuments(filepath.Join(r.ScratchDir(n.From), publishStage), listed, docs); err != nil {
		return err
	}
	err = scan.Run(ctx, inDir{runner, publishStage}, listed)
	var found *scan.FoundError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &found):
		return &hardStop{gate: gungnir.GateRealSecrets, err: err}
	}
	return &nodeFailure{err}
}

// stageDocuments makes stage anew and writes each document's bytes in it, at its path, as a new file.
func stageDocuments(stage string, files []string, docs [][]byte) error {
	if err := emptyDir(stage); err != nil {
		return err
	}
	for i, f := range files {
		p := filepath.Join(stage, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return err
		}
		_, err = w.Write(docs[i])
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// inDir runs every command in dir, a folder of the runner's workspace.
type inDir struct {
	gungnir.Runner
	dir string
}

func (d inDir) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	c.Dir = path.Join(d.dir, c.Dir)
	return d.Runner.Run(ctx, c)
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

// copyDocument writes data, a document as readDocuments read it, to dst, making dst's folders. Nothing on
// the way to dst may be a symlink: a document must land where the node says, inside the project.
func copyDocument(project string, data []byte, dst string) error {
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
