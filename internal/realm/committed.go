package realm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/ouroboric-dev/odin/internal/git"
)

type blob struct {
	mode, sha, name string
	size            int64
}

// hashCommitted hashes a realm folder in a git work tree from its committed files at HEAD, read with
// ls-tree and cat-file as the snapshot export reads them, in the format and order the folder walk uses, so a
// clean checkout hashes as it did before. gitDir is the repo's git dir, top its work tree's top, and prefix
// the folder's path from top with a trailing slash ("" when the folder is the top): only the files under the
// folder count, named relative to it, and a change to a file elsewhere in the repo does not matter. A folder
// with no committed file is refused, not hashed as an empty one. A run reads the realm from its folder, so
// a tracked file that differs from HEAD is refused: it would be read but not hashed. A committed symlink, or any entry that is not
// a regular file, is refused as in a plain folder.
func hashCommitted(gitDir, top, prefix, dir string, lim limits) (string, error) {
	ctx := context.Background()
	repo := git.Repo{GitDir: gitDir, WorkTree: top}
	// A pathspec of "" would match nothing; the top is ".".
	spec := strings.TrimSuffix(prefix, "/")
	if spec == "" {
		spec = "."
	}
	changed, err := repo.Output(ctx, "status", "--porcelain", "-z", "--untracked-files=no", "--", ":(literal)"+spec)
	if err != nil {
		return "", err
	}
	if len(changed) > 0 {
		var names []string
		for _, rec := range bytes.Split(changed, []byte{0}) {
			if len(rec) > 3 {
				names = append(names, string(rec[3:]))
			}
		}
		return "", fmt.Errorf("%s: %s differs from the committed realm; commit it or undo it, since a run reads the folder and the realm's hash is of the commit", dir, strings.Join(names, ", "))
	}
	out, err := repo.Output(ctx, "ls-tree", "-r", "-l", "-z", "HEAD", "--", spec)
	if err != nil {
		return "", err
	}
	var blobs []blob
	var size int64
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := strings.Cut(string(rec), "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return "", fmt.Errorf("git ls-tree gave an entry that is not understood: %q", rec)
		}
		b := blob{mode: f[0], sha: f[2], name: strings.TrimPrefix(name, prefix)}
		switch b.mode {
		case "100644", "100755":
		case "120000":
			return "", fmt.Errorf("%s/%s is a symlink; a realm holds its own files and folders, and a link could lead out of it", dir, name)
		default:
			return "", fmt.Errorf("%s/%s is a git entry of mode %s, not a regular file", dir, name, b.mode)
		}
		if b.size, err = strconv.ParseInt(f[3], 10, 64); err != nil {
			return "", fmt.Errorf("git ls-tree gave a size that is not understood: %q", rec)
		}
		if int64(len(blobs)+1) > lim.files {
			return "", fmt.Errorf("%s holds more than %d files; a realm is a few hundred small ones, so this is not one", dir, lim.files)
		}
		if size += b.size; size > lim.bytes {
			return "", fmt.Errorf("%s holds more than %d bytes; a realm is text and small, so this is not one", dir, lim.bytes)
		}
		blobs = append(blobs, b)
	}
	if len(blobs) == 0 {
		return "", fmt.Errorf("%s: none of its files is committed (untracked or git-ignored files are not part of a realm's hash); "+
			"commit them", dir)
	}
	// The folder walk visits a folder's entries in name order, so "a/b" comes before "a.b".
	slices.SortFunc(blobs, func(a, b blob) int {
		return slices.Compare(strings.Split(a.name, "/"), strings.Split(b.name, "/"))
	})
	var want strings.Builder
	for _, b := range blobs {
		want.WriteString(b.sha + "\n")
	}
	h := sha256.New()
	err = repo.PipeIn(ctx, strings.NewReader(want.String()), func(r io.Reader) error {
		br := bufio.NewReader(r)
		for _, b := range blobs {
			head, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			f := strings.Fields(head)
			if len(f) != 3 || f[0] != b.sha || f[1] != "blob" || f[2] != strconv.FormatInt(b.size, 10) {
				return fmt.Errorf("git cat-file gave %q for %s", strings.TrimSpace(head), b.name)
			}
			mode := "file"
			if b.mode == "100755" {
				mode = "exec"
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00", mode, b.name, b.size)
			if _, err := io.CopyN(h, br, b.size); err != nil {
				return err
			}
			if _, err := br.Discard(1); err != nil {
				return err
			}
		}
		return nil
	}, "cat-file", "--batch")
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
