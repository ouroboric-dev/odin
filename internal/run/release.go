package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/odin/internal/snapshot"
)

// What a release may hold. A release is a built product (a binary, a bundle), so the bounds are wider than
// the snapshot's for source (32 MiB a file, 256 MiB in all). The real releases measure 15 MiB (the API
// binary) and under 1 MiB (the web bundle); a later commit adjusts the bounds if a size comes near.
const (
	maxReleaseFileBytes  int64 = 128 << 20
	maxReleaseTotalBytes int64 = 512 << 20
)

// The bounds as variables, so that a test can narrow them.
var (
	maxReleaseFile  = maxReleaseFileBytes
	maxReleaseTotal = maxReleaseTotalBytes
)

// releaseScript is the file a leaf holds, executable, when it has a release (spec: the release in the
// export). The leaf's realm decides what it builds.
const releaseScript = "release.sh"

// Releaser builds the releases of the leaves of a snapshot. snapshot is the exported project; leaves are
// the paths of the leaves that hold an executable release.sh. It returns a folder that holds each leaf's
// release at the leaf's path (<dir>/repos/api/...). The folder is the releaser's: Odin copies it, under
// the bounds above, and never keeps it. Anything the folder holds outside the leaves' paths is ignored.
type Releaser interface {
	Build(ctx context.Context, snapshot string, leaves []string) (dir string, err error)
}

// ReleaseError is a release that could not be made or that the export refuses. It names the leaves.
type ReleaseError struct {
	Leaves []string
	Err    error
}

func (e *ReleaseError) Error() string {
	return fmt.Sprintf("the release of %s could not be made: %v", strings.Join(e.Leaves, ", "), e.Err)
}
func (e *ReleaseError) Unwrap() error { return e.Err }

// releaseDir is where the releases of a run are kept, each under the key of the commits it was built from.
func (r *Run) releaseDir() string { return filepath.Join(r.Dir, "release") }

// addRelease puts the release of each leaf of sources that has one into the export at dst, under
// .ygg/release/<leaf path>/. A release is built once for a set of commits and kept in the run folder; an
// export of the same commits copies it again. Nothing is committed to the project or a leaf.
func (r *Run) addRelease(ctx context.Context, dst string, sources []snapshot.Source) error {
	if r.Releaser == nil {
		return nil
	}
	var leaves []string
	key := sha256.New()
	for _, s := range sources {
		rev := s.Rev
		if rev == "" {
			out, err := s.Repo.Run(ctx, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			rev = strings.TrimSpace(out)
		}
		fmt.Fprintf(key, "%s %s\n", s.Rel, rev)
		if s.Rel == "." {
			continue
		}
		out, err := s.Repo.Run(ctx, "ls-tree", rev, "--", releaseScript)
		if err != nil {
			return err
		}
		if strings.HasPrefix(out, "100755 ") {
			leaves = append(leaves, filepath.ToSlash(s.Rel))
		}
	}
	if len(leaves) == 0 {
		return nil
	}
	slices.Sort(leaves)
	kept := filepath.Join(r.releaseDir(), hex.EncodeToString(key.Sum(nil))[:32])
	if _, err := os.Stat(kept); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.buildRelease(ctx, dst, kept, leaves); err != nil {
			return &ReleaseError{leaves, err}
		}
	}
	for _, leaf := range leaves {
		if err := copyRelease(filepath.Join(kept, filepath.FromSlash(leaf)), filepath.Join(dst, ".ygg", "release", filepath.FromSlash(leaf)), nil); err != nil {
			return &ReleaseError{[]string{leaf}, err}
		}
	}
	return nil
}

// buildRelease has the releaser build the leaves from the export at dst, checks what it made and keeps a
// copy at kept. A failure leaves nothing at kept, and the releaser's message in release.log, beside
// grade.log.
func (r *Run) buildRelease(ctx context.Context, dst, kept string, leaves []string) (err error) {
	defer func() {
		if err != nil && ctx.Err() == nil {
			if merr := os.MkdirAll(r.GradeDir(), 0o755); merr == nil {
				os.WriteFile(filepath.Join(r.GradeDir(), "release.log"), []byte(err.Error()+"\n"), 0o600)
			}
		}
	}()
	out, err := r.Releaser.Build(ctx, dst, leaves)
	if err != nil {
		return err
	}
	tmp := kept + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var total int64
	for _, leaf := range leaves {
		if err := copyRelease(filepath.Join(out, filepath.FromSlash(leaf)), filepath.Join(tmp, filepath.FromSlash(leaf)), &total); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("%s: %w", leaf, err)
		}
	}
	if err := os.Rename(tmp, kept); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// copyRelease copies the folder from to to. Only regular files and folders are copied: a symlink, a device
// or any other entry is refused, and so is a file past the per-file bound, or a total (counted into total,
// when it is not nil) past the bound of all. A release is checked whole before a byte is copied.
func copyRelease(from, to string, total *int64) error {
	if total == nil {
		total = new(int64)
	}
	if err := checkRelease(from, total); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, info.Mode().Perm()&0o755|0o600)
	})
}

// checkRelease walks the folder and refuses what a release may not hold, adding the bytes it holds to total.
func checkRelease(dir string, total *int64) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the releaser made no release: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the release is not a folder")
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case !t.IsRegular():
			return fmt.Errorf("%s is a %s; a release holds only files and folders", rel, kindOf(t))
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxReleaseFile {
			return fmt.Errorf("%s is %d bytes; a release file may hold %d", rel, info.Size(), maxReleaseFile)
		}
		if *total += info.Size(); *total > maxReleaseTotal {
			return fmt.Errorf("the release holds more than %d bytes", maxReleaseTotal)
		}
		return nil
	})
}

func kindOf(t fs.FileMode) string {
	switch {
	case t&fs.ModeSymlink != 0:
		return "symlink"
	case t&fs.ModeDevice != 0:
		return "device file"
	default:
		return "special file"
	}
}

func copyFile(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
