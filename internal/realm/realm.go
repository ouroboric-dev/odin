// Package realm loads realm leaves (change realm-leaf-naming): the repos that hold a pack.yaml with a
// graph, prompts, templates and checks. A run loads each leaf's realm once, at the
// version the project pins, and records a hash of each so a resume can tell nothing changed under it.
package realm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
)

// Realm is a realm leaf as a run loaded it.
type Realm struct {
	Name    string
	Version string
	Dir     string // absolute
	Hash    string // of every file a run could read; see Hash
	Pack    *schema.Pack
	// Stack is the image the realm's leaves start on: the pinned image its pack names, or the local tag of
	// the image `ygg sandbox build` builds from the realm's build context. Acceptance is the same for the
	// stack acceptance checks run on. Both are empty for a realm with no sandbox stack.
	Stack, Acceptance string
}

// Load hashes the realm in dir and reads its pack. The hash comes first: it refuses a symlink anywhere in
// the realm before anything in it is read.
func Load(dir string) (*Realm, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	h, err := Hash(abs)
	if err != nil {
		return nil, err
	}
	p, err := schema.LoadPack(abs)
	if err != nil {
		return nil, err
	}
	r := &Realm{Name: p.Name, Version: p.Version, Dir: abs, Hash: h, Pack: p}
	if r.Stack, err = p.StackRef(abs); err != nil {
		return nil, err
	}
	if r.Acceptance, err = p.AcceptanceRef(abs); err != nil {
		return nil, err
	}
	return r, nil
}

type limits struct{ files, bytes int64 }

// A realm is a few hundred small files. One with more is not a realm but a folder that holds build
// output, and hashing it would be slow and its hash would change with every build.
var defaultLimits = limits{files: 5000, bytes: 64 << 20}

// Hash returns a SHA-256 over every file of the realm but the contents of .git: each file's path, its
// executable bit and its bytes, in path order. A realm folder in a git work tree, the repo's own or a
// folder of it, is its committed files at HEAD (see hashCommitted): git-ignored build output and untracked
// files are not part of it (B-050), and a folder with no committed file is refused. Any other folder is hashed as it is on disk. A run records it, and a resume refuses a realm whose hash
// changed (spec §4.3: the run's positions mean what they meant). A realm holds regular files and folders
// only: a symlink anywhere in it is refused, since a run reads prompts, first-commit files and the
// template by following links, so a link could read outside the realm, and a change at its target would
// not change the hash (review M-7).
func Hash(dir string) (string, error) { return hashLimited(dir, defaultLimits) }

func hashLimited(dir string, lim limits) (string, error) {
	// The walk does not enter a root that is a link, so its hash would be that of an empty folder.
	if st, err := os.Lstat(dir); err != nil {
		return "", err
	} else if st.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink; a realm holds its own files and folders, and a link could lead out of it", dir)
	}
	// A folder inside a git work tree, whether it holds the repo's .git or is one folder of the repo (a graph
	// pack in the voluspa leaf), is its committed files. A folder with a .git that git refuses (an invalid
	// file, or a repo it finds dubious) is refused too, not hashed from disk. A folder with no .git that is
	// outside any work tree (git says "not a git repository") is a plain folder; any other refusal of git, for a repo
	// that holds the folder, is final.
	gitDir, top, prefix, gitErr := workTreeOf(dir)
	_, statErr := os.Lstat(filepath.Join(dir, ".git"))
	switch {
	case gitErr == nil:
		return hashCommitted(gitDir, top, prefix, dir, lim)
	case statErr == nil:
		return "", fmt.Errorf("%s holds a .git that git does not accept, so its files can't be hashed from the commit: %w",
			dir, gitErr)
	case !strings.Contains(gitErr.Error(), "not a git repository"):
		// git refuses a repo that holds dir (for example for its owner: Odin sets GIT_CONFIG_GLOBAL=/dev/null,
		// so a global safe.directory does not apply). Its files could be the repo's, so fail closed.
		return "", fmt.Errorf("%s is inside a git repository that git does not accept, so its files can't be hashed from the commit: %w",
			dir, gitErr)
	}
	h := sha256.New()
	var files, size int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; a realm holds its own files and folders, and a link could lead out of it", p)
		}
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		files++
		if files > lim.files {
			return fmt.Errorf("%s holds more than %d files; a realm is a few hundred small ones, so this is not one", dir, lim.files)
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is %s, not a regular file", p, d.Type())
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		if size > lim.bytes {
			return fmt.Errorf("%s holds more than %d bytes; a realm is text and small, so this is not one", dir, lim.bytes)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := "file"
		if info.Mode()&0o111 != 0 {
			mode = "exec"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", mode, rel, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Store is a folder with one checkout of each leaf of a kind: <Dir>/<name>/pack.yaml. Kind is "realm" (factory.yaml,
// realms.dir) or "graph" (graphs.dir); it names the store in messages and picks the name rule.
type Store struct{ Dir, Kind string }

// Find returns the folder of the pack called name. The pack in it must say it is that realm or graph.
func (s *Store) Find(name string) (string, error) {
	valid := gungnir.ValidRealm
	if s.Kind == "graph" {
		valid = gungnir.ValidGraph
	}
	if !valid(name) {
		return "", fmt.Errorf("%q is not a %s name (lower case words joined by dashes)", name, s.Kind)
	}
	dir := filepath.Join(s.Dir, name)
	p, err := schema.LoadPack(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("there is no %s %s in %s (factory.yaml, %ss.dir)", s.Kind, name, s.Dir, s.Kind)
	case err != nil:
		return "", err
	case p.Name != name:
		return "", fmt.Errorf("%s holds the %s %s, not %s", dir, s.Kind, p.Name, name)
	}
	return dir, nil
}

// Resolve turns what --realm or --graph says (kind is "realm" or "graph") into a folder. A value with a path
// separator, or one that starts with a dot, is a path: a pack checked out anywhere, used as it is. Anything
// else is a name, found in the store; byName is true for it. A name with no store is an error that says
// where to set one.
func Resolve(ref string, store *Store, kind string) (dir string, byName bool, err error) {
	if strings.ContainsRune(ref, filepath.Separator) || strings.HasPrefix(ref, ".") {
		return ref, false, nil
	}
	if store == nil {
		return "", true, fmt.Errorf("--%s %s names a %s leaf, but factory.yaml has no %ss section to find it in: set %ss: {dir: <folder with the %s leaves>}, or give --%s a path",
			kind, ref, kind, kind, kind, kind, kind)
	}
	dir, err = store.Find(ref)
	return dir, true, err
}

// workTreeOf says where dir stands in a git work tree: the repo's git dir, the work tree's top, and dir's
// path from the top with a trailing slash ("" at the top). The error is git's, for a folder outside any work
// tree or in one git refuses.
func workTreeOf(dir string) (gitDir, top, prefix string, err error) {
	out, err := git.Run(context.Background(), dir, "rev-parse", "--absolute-git-dir", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || lines[0] == "" || lines[1] == "" {
		return "", "", "", fmt.Errorf("git rev-parse gave %q, not a git dir and a work tree", out)
	}
	if len(lines) > 2 {
		prefix = lines[2]
	}
	return lines[0], lines[1], prefix, nil
}
