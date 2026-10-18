package deps

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
// byte. It skips .git (the leaf's git directory is the leaf's own, and the bump never writes to it) and
// node_modules (a cache npm rebuilds from the lock file).
type Snapshot struct {
	root  string
	files map[string]entry
	dirs  map[string]bool
}

func skipped(rel string, d fs.DirEntry) bool {
	n := d.Name()
	return rel != "." && (n == ".git" || (n == "node_modules" && d.IsDir()))
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
	s := &Snapshot{root: root, files: map[string]entry{}, dirs: map[string]bool{}}
	var size int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		e, n, err := read(p, d)
		if err != nil {
			return err
		}
		if size += n; size > limit {
			return fmt.Errorf("%s holds too many bytes of files: too big to snapshot, so a failed bump could not be undone", root)
		}
		s.files[rel] = e
		return nil
	})
	return s, err
}

func read(p string, d fs.DirEntry) (entry, int64, error) {
	info, err := d.Info()
	if err != nil {
		return entry{}, 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		return entry{mode: info.Mode(), link: l}, 0, err
	case info.Mode().IsRegular():
		b, err := os.ReadFile(p)
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
		cur, _, err := read(p, d)
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
	var extra, extraDirs []string
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
	})
	if err != nil {
		return err
	}
	for _, rel := range extra {
		if err := os.Remove(filepath.Join(s.root, rel)); err != nil {
			return err
		}
	}
	for rel, want := range s.files {
		p := filepath.Join(s.root, rel)
		if info, err := os.Lstat(p); err == nil {
			if cur, _, err := read(p, fs.FileInfoToDirEntry(info)); err == nil && same(want, cur) {
				continue
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
		if want.link != "" {
			err = os.Symlink(want.link, p)
		} else if err = os.WriteFile(p, want.data, want.mode); err == nil {
			err = os.Chmod(p, want.mode)
		}
		if err != nil {
			return err
		}
	}
	slices.SortFunc(extraDirs, func(a, b string) int { return strings.Compare(b, a) }) // the deepest first
	for _, rel := range extraDirs {
		os.Remove(filepath.Join(s.root, rel)) // a folder that still holds something is not ours to remove
	}
	return nil
}
