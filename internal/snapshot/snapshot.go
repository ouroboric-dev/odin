// Package snapshot exports a project as it would be built: the committed files of the project umbrella
// and of each leaf, at the commits they are on, into a plain folder with no git directory (spec §6.1,
// change realm-content). Acceptance checks run on it in a sandbox of their own, where no leaf's
// working copy, git dir or ignored build output can reach them or be changed by them.
package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ouroboric-dev/odin/internal/git"
)

// What a snapshot may hold. A project's committed files are source code, so these are generous; a repo
// with more is holding something that is not source, and the export says so rather than fill the disk.
var (
	maxFiles       = 20000
	maxBytes int64 = 256 << 20
	maxFile  int64 = 32 << 20
)

// Source is one repo of the snapshot: where it goes, relative to the snapshot's top ("." for the project
// umbrella), the repo to take the files of, always with its git dir given explicitly, and the commit to
// take them at: HEAD when Rev is empty, otherwise a full commit hash.
type Source struct {
	Rel  string
	Repo git.Repo
	Rev  string
}

var revPattern = regexp.MustCompile(`^(HEAD|[0-9a-f]{40})$`)

// Export makes dst hold the committed files of each source, at its commit, in the order given. dst stays:
// its contents are removed in place, since a sandbox that mounted it keeps seeing the same folder. A
// tracked symlink, or any entry that is not a regular file or a folder, is refused, and so are a file
// that two sources both hold, a path that leaves its source's folder, and more files or bytes than the
// limits. A failed export leaves dst partly written: the caller does not use it.
func Export(ctx context.Context, dst string, sources []Source) error {
	if err := emptyDir(dst); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	var files int
	var total int64
	for _, s := range sources {
		if s.Rel != "." && (s.Rel == "" || !filepath.IsLocal(s.Rel)) {
			return fmt.Errorf("a snapshot source at %q: it must be a path inside the snapshot", s.Rel)
		}
		rev := s.Rev
		if rev == "" {
			rev = "HEAD"
		}
		if !revPattern.MatchString(rev) {
			return fmt.Errorf("a snapshot source at %q: %q is neither HEAD nor a full commit hash", s.Rel, s.Rev)
		}
		base := filepath.ToSlash(filepath.Clean(s.Rel))
		err := exportOne(ctx, root, s, rev, base, &files, &total)
		if err != nil {
			return fmt.Errorf("exporting %s: %w", s.Rel, err)
		}
	}
	return nil
}

// entry is one line of git ls-tree: a file, or a submodule's commit, of the committed tree.
type entry struct {
	mode, sha, name string
	size            int64
}

// exportOne writes the files of s at rev under base. It reads the committed tree itself, with ls-tree
// and cat-file, and not with git archive: archive obeys the .gitattributes of the tree it archives,
// where export-ignore drops committed files and export-subst rewrites their contents.
func exportOne(ctx context.Context, root *os.Root, s Source, rev, base string, files *int, total *int64) error {
	out, err := s.Repo.Output(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", rev)
	if err != nil {
		return err
	}
	var blobs []entry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := strings.Cut(string(rec), "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return fmt.Errorf("git ls-tree gave an entry that is not understood: %q", rec)
		}
		e := entry{mode: f[0], sha: f[2], name: path.Clean(name)}
		if !filepath.IsLocal(e.name) {
			return fmt.Errorf("%s holds a path that leaves the snapshot: %q", s.Rel, name)
		}
		target := path.Join(base, e.name)
		switch e.mode {
		case "160000":
			if err := root.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case "100644", "100755":
			if e.size, err = strconv.ParseInt(f[3], 10, 64); err != nil {
				return fmt.Errorf("git ls-tree gave a size that is not understood: %q", rec)
			}
			if *files++; *files > maxFiles {
				return fmt.Errorf("the snapshot would hold more than %d files", maxFiles)
			}
			if *total += e.size; e.size > maxFile || *total > maxBytes {
				return fmt.Errorf("the snapshot would hold more than %d bytes (%s is %d)", maxBytes, e.name, e.size)
			}
			blobs = append(blobs, e)
		default:
			return fmt.Errorf("%s holds %s, which is not a regular file; a link could lead the snapshot's checks outside it", target, e.name)
		}
	}
	var want strings.Builder
	for _, e := range blobs {
		want.WriteString(e.sha + "\n")
	}
	return s.Repo.PipeIn(ctx, strings.NewReader(want.String()), func(r io.Reader) error {
		br := bufio.NewReader(r)
		for _, e := range blobs {
			head, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			f := strings.Fields(head)
			if len(f) != 3 || f[0] != e.sha || f[1] != "blob" || f[2] != strconv.FormatInt(e.size, 10) {
				return fmt.Errorf("git cat-file gave %q for %s", strings.TrimSpace(head), e.name)
			}
			target := path.Join(base, e.name)
			if err := root.MkdirAll(path.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if e.mode == "100755" {
				mode = 0o755
			}
			f2, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return fmt.Errorf("%s: %w; a file that two repos both hold is refused", target, err)
			}
			_, err = io.CopyN(f2, br, e.size)
			if cerr := f2.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if _, err := br.Discard(1); err != nil {
				return err
			}
		}
		return nil
	}, "cat-file", "--batch")
}

// emptyDir removes everything in dir and keeps dir; a dir that is not there is made.
func emptyDir(dir string) error {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s is not a folder", dir)
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
