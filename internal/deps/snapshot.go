package deps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// MaxSnapshot is the most bytes of files a snapshot keeps. A product bigger than this is refused: the
// bump could not undo a failure.
const MaxSnapshot int64 = 256 << 20

type entry struct {
	mode fs.FileMode
	data []byte
	link string // a symlink's target
}

// Snapshot holds the files of a product as they were, so that a failed bump can put them back byte for
// byte. It skips the top-level .git (the leaf's git directory is the leaf's own, and the bump never writes to it) and
// node_modules (a cache npm rebuilds from the lock file).
type Snapshot struct {
	root  string
	files map[string]entry
	dirs  map[string]bool
	limit int64
	git   gitState // the top-level .git, which the walks skip but the bump still watches
}

// maxGitFile bounds what is read of the leaf's .git: git writes one short line.
const maxGitFile = 4096

// gitState is the top-level .git as it was: absent, a directory, a symlink, or a file and its bytes.
type gitState struct {
	kind string // absent, dir, link, file, other
	data string // the file's bytes, or the link's target
	mode fs.FileMode
}

func readGit(root string) (gitState, error) {
	p := filepath.Join(root, ".git")
	info, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return gitState{kind: "absent"}, nil
	case err != nil:
		return gitState{}, err
	case info.IsDir():
		return gitState{kind: "dir"}, nil
	case info.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		return gitState{kind: "link", data: l}, err
	case !info.Mode().IsRegular():
		return gitState{kind: "other"}, nil
	case info.Size() > maxGitFile:
		return gitState{kind: "file", data: fmt.Sprintf("a file of %d bytes", info.Size()), mode: info.Mode().Perm()}, nil
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return gitState{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxGitFile+1))
	return gitState{kind: "file", data: string(b), mode: info.Mode().Perm()}, err
}

// GitChanged reports whether the top-level .git is not as it was: a rewritten .git file is how code in a
// leaf points the operator's own git at a repository it made.
func (s *Snapshot) GitChanged() (bool, error) {
	now, err := readGit(s.root)
	return now != s.git, err
}

// restoreGit puts the .git file back: the name is replaced by a new file and never written through.
func (s *Snapshot) restoreGit() error {
	now, err := readGit(s.root)
	if err != nil {
		return err
	}
	if now == s.git {
		return nil
	}
	p := filepath.Join(s.root, ".git")
	switch s.git.kind {
	case "absent":
		return os.RemoveAll(p)
	case "file":
		if len(s.git.data) > 0 && strings.HasPrefix(s.git.data, "a file of ") {
			return fmt.Errorf("%s: the .git file was not a git pointer and cannot be put back", s.root)
		}
		f, err := os.CreateTemp(s.root, ".git.ygg-*")
		if err != nil {
			return err
		}
		_, err = f.WriteString(s.git.data)
		if err == nil {
			err = f.Chmod(s.git.mode)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			if info, lerr := os.Lstat(p); lerr == nil && info.IsDir() {
				err = os.RemoveAll(p) // a repository the bump's code planted where the file was
			}
		}
		if err == nil {
			err = os.Rename(f.Name(), p)
		}
		if err != nil {
			os.Remove(f.Name())
		}
		return err
	}
	return fmt.Errorf("%s: the .git (%s) cannot be put back", s.root, s.git.kind)
}

func skipped(rel string, d fs.DirEntry) bool {
	n := d.Name()
	return rel != "." && ((rel == ".git" && n == ".git") || (n == "node_modules" && d.IsDir()))
}

// skip is what a walk returns for an entry it skips: SkipDir on a file would skip its folder's other files.
func skip(d fs.DirEntry) error {
	if d.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// Take reads every file of the product in root.
func Take(root string) (*Snapshot, error) { return takeLimited(root, MaxSnapshot) }

func takeLimited(root string, limit int64) (*Snapshot, error) {
	s := &Snapshot{root: root, files: map[string]entry{}, dirs: map[string]bool{}, limit: limit}
	var size int64
	var err error
	if s.git, err = readGit(root); err != nil {
		return s, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case skipped(rel, d):
			return skip(d)
		case d.IsDir():
			s.dirs[rel] = true
			return nil
		}
		e, n, err := read(p, d, limit-size)
		if err != nil {
			return err
		}
		if size += n; size > limit {
			return errTooBig(root)
		}
		s.files[rel] = e
		return nil
	})
	return s, err
}

func errTooBig(root string) error {
	return fmt.Errorf("%s holds too many bytes of files: too big to snapshot, so a failed bump could not be undone", root)
}

// read reads a file for the snapshot or a comparison with it. A file bigger than limit is refused from its
// size, before a byte is read (a sparse file is a terabyte of memory), and the read stops at the limit.
func read(p string, d fs.DirEntry, limit int64) (entry, int64, error) {
	info, err := d.Info()
	if err != nil {
		return entry{}, 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		return entry{mode: info.Mode(), link: l}, 0, err
	case info.Mode().IsRegular():
		if info.Size() > limit {
			return entry{}, 0, errTooBig(filepath.Dir(p))
		}
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return entry{}, 0, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err == nil && int64(len(b)) > limit {
			err = errTooBig(filepath.Dir(p))
		}
		return entry{mode: info.Mode().Perm(), data: b}, int64(len(b)), err
	}
	return entry{}, 0, fmt.Errorf("%s is neither a file, a folder nor a symlink", p)
}

// Changes lists the files, relative to the product, that differ from the snapshot: changed, created or
// removed. The list is sorted.
func (s *Snapshot) Changes() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.root, p)
		switch {
		case skipped(rel, d):
			return skip(d)
		case d.IsDir():
			return nil
		}
		seen[rel] = true
		cur, _, err := read(p, d, s.limit)
		if err != nil {
			return err
		}
		if want, ok := s.files[rel]; !ok || !same(want, cur) {
			out = append(out, rel)
		}
		return nil
	})
	for rel := range s.files {
		if !seen[rel] {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, err
}

func same(a, b entry) bool {
	return a.mode == b.mode && a.link == b.link && bytes.Equal(a.data, b.data)
}

// Restore puts the product back as the snapshot was: it rewrites changed files and the ones removed, and
// removes the files and the empty folders that were created. File times are not restored.
func (s *Snapshot) Restore() error {
	// The .git file first, and on its own: it is the one thing that makes the operator's own git run code of
	// the bump, so nothing below may stand in its way. Every step after it goes on past an error and the
	// errors are joined.
	errs := []error{s.restoreGit()}
	// Folders the code made unreadable are opened up, and a repository planted below node_modules is removed.
	errs = append(errs, openUp(s.root, "."))
	var extra, extraDirs []string
	errs = append(errs, filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				errs = append(errs, err)
				return skip(d) // a folder that can't be read is reported, and the rest is still put back
			}
			return err
		}
		rel, _ := filepath.Rel(s.root, p)
		switch {
		case skipped(rel, d):
			return skip(d)
		case d.IsDir():
			if !s.dirs[rel] {
				extraDirs = append(extraDirs, rel)
			}
			return nil
		}
		if _, ok := s.files[rel]; !ok {
			extra = append(extra, rel)
		}
		return nil
	}))
	for _, rel := range extra {
		if err := os.Remove(filepath.Join(s.root, rel)); err != nil {
			errs = append(errs, err)
		}
	}
	for rel, want := range s.files {
		if err := s.putBack(rel, want); err != nil {
			errs = append(errs, err)
		}
	}
	slices.SortFunc(extraDirs, func(a, b string) int { return strings.Compare(b, a) }) // the deepest first
	for _, rel := range extraDirs {
		p := filepath.Join(s.root, rel)
		// Only what is still a folder, and is not a file of the snapshot: the files loop may have put a file where
		// the check made a folder, and removing that name would delete the file just restored.
		if info, err := os.Lstat(p); err != nil || !info.IsDir() {
			if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) { // gone, or its parent is a file now
				errs = append(errs, err)
			}
			continue
		}
		if _, isFile := s.files[rel]; isFile {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, syscall.ENOTEMPTY) { // a folder that still holds something is not ours to remove
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// putBack writes one file of the snapshot back if it differs.
func (s *Snapshot) putBack(rel string, want entry) error {
	p := filepath.Join(s.root, rel)
	if info, err := os.Lstat(p); err == nil {
		if cur, _, err := read(p, fs.FileInfoToDirEntry(info), s.limit); err == nil && same(want, cur) {
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var err error
	if want.link != "" {
		err = os.Symlink(want.link, p)
	} else if err = os.WriteFile(p, want.data, want.mode); err == nil {
		err = os.Chmod(p, want.mode)
	}
	return err
}

// openUp walks the folders of the product (not the top-level .git, not through a symlink), gives the owner
// access to each one the code made unreadable, and removes a .git below a node_modules folder. It goes on
// past an error and returns them all.
func openUp(root, rel string) error {
	dir := filepath.Join(root, rel)
	var errs []error
	if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode().Perm()&0o700 != 0o700 {
		if err := os.Chmod(dir, info.Mode().Perm()|0o700); err != nil {
			errs = append(errs, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(rel, e.Name())
		switch {
		case child == ".git":
			continue
		case e.Name() == ".git" && rel != ".":
			if strings.Contains(filepath.ToSlash(rel)+"/", "/node_modules/") || strings.HasPrefix(filepath.ToSlash(rel)+"/", "node_modules/") {
				errs = append(errs, os.RemoveAll(filepath.Join(root, child)))
				continue
			}
		}
		if e.IsDir() {
			errs = append(errs, openUp(root, child))
		}
	}
	return errors.Join(errs...)
}
