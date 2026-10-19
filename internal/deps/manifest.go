package deps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Kind is the ecosystem of a manifest.
type Kind string

const (
	Go  Kind = "go"  // go.mod
	NPM Kind = "npm" // package.json
)

// Manifest is where a product requires a package.
type Manifest struct {
	Kind    Kind
	Path    string  // the manifest file
	Current Version // the version it requires now
	Tilde   bool    // npm: the requirement is written ~x.y.z, so the bump keeps the tilde
}

// Files are the files, relative to the manifest's folder, that the ecosystem tool may change.
func (k Kind) Files() []string {
	if k == Go {
		return []string{"go.mod", "go.sum"}
	}
	return []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"}
}

// Command is the ecosystem tool's command that moves pkg to version to. The command takes no shell. npm
// runs no install script, and keeps the pin style the manifest has: exact, or a tilde.
func (m Manifest) Command(pkg string, to Version) []string {
	if m.Kind == Go {
		return []string{"env", "GOWORK=off", "go", "get", pkg + "@v" + to.String()} // GOWORK=off: no go.work of a parent folder moves the bump
	}
	style := []string{"--save-exact"} // an .npmrc with save-exact=true would turn a tilde off, so a tilde says both
	if m.Tilde {
		style = []string{"--save-exact=false", "--save-prefix=~"}
	}
	return append(append([]string{"npm", "install", "--ignore-scripts"}, style...), pkg+"@"+to.String())
}

// Find reads the manifest of the product in dir that requires pkg. A package that no manifest requires,
// that both manifests require, or whose version it can't read (a range, a URL, a tag) is an error: a
// bump never adds a package and never guesses.
func Find(dir, pkg string) (Manifest, error) {
	var found []Manifest
	for _, read := range []func(string, string) (Manifest, bool, error){readGoMod, readPackageJSON} {
		m, ok, err := read(dir, pkg)
		if err != nil {
			return Manifest{}, err
		}
		if ok {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return Manifest{}, fmt.Errorf("%s requires no package %s in its go.mod or package.json", dir, pkg)
	case 1:
		return found[0], nil
	}
	return Manifest{}, fmt.Errorf("%s requires %s in both go.mod and package.json; the bump will not guess", dir, pkg)
}

// maxManifest is the most bytes a manifest may hold: a leaf's code can swap the file for anything.
const maxManifest = 4 << 20

// readManifest reads a manifest of a leaf, whose files are data: a file that is not a regular file (a
// symlink, a named pipe, a device) is refused before it is opened, the open follows no link and waits on
// no pipe, and the read stops at maxManifest.
func readManifest(path string) ([]byte, error) { return readBounded(path, maxManifest) }

// maxLock is the most bytes freeze reads of a manifest or lock file.
const maxLock = 64 << 20

func readBounded(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s holds %d bytes, over the %d bytes allowed", path, info.Size(), max)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s holds over %d bytes", path, max)
	}
	return b, err
}

func readGoMod(dir, pkg string) (Manifest, bool, error) {
	path := filepath.Join(dir, "go.mod")
	b, err := readManifest(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	} else if err != nil {
		return Manifest{}, false, err
	}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "require" && f[1] == "(":
			in = true
			continue
		case in && len(f) == 1 && f[0] == ")":
			in = false
			continue
		case len(f) == 3 && f[0] == "require" && !in:
			f = f[1:]
		case !in || len(f) != 2:
			continue
		}
		if f[0] != pkg {
			continue
		}
		v, err := ParseVersion(f[1])
		if err != nil || v.Pre != "" {
			return Manifest{}, false, fmt.Errorf("%s requires %s %s: that is not a tagged release, so the bump will not guess", path, pkg, f[1])
		}
		return Manifest{Kind: Go, Path: path, Current: v}, true, nil
	}
	return Manifest{}, false, nil
}

func readPackageJSON(dir, pkg string) (Manifest, bool, error) {
	path := filepath.Join(dir, "package.json")
	b, err := readManifest(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	} else if err != nil {
		return Manifest{}, false, err
	}
	var doc struct {
		Dependencies, DevDependencies, OptionalDependencies map[string]string
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Manifest{}, false, fmt.Errorf("%s: %w", path, err)
	}
	for _, section := range []map[string]string{doc.Dependencies, doc.DevDependencies, doc.OptionalDependencies} {
		spec, ok := section[pkg]
		if !ok {
			continue
		}
		tilde := strings.HasPrefix(spec, "~")
		v, err := ParseVersion(strings.TrimLeft(spec, "~=v"))
		if err != nil || v.Pre != "" || strings.HasPrefix(spec, "^") {
			return Manifest{}, false, fmt.Errorf("%s requires %s as %q: the policy pins x.y.z or ~x.y.z, so the bump will not guess", path, pkg, spec)
		}
		return Manifest{Kind: NPM, Path: path, Current: v, Tilde: tilde}, true, nil
	}
	return Manifest{}, false, nil
}
