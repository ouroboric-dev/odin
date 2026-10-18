package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
)

// Limits on what the generator reads. A leaf is written by an agent, so the generator never follows a
// link out of it and never reads without a bound.
const maxFile = 1 << 20 // bytes of one file

// Result is a generated wiring: its edges, and what the generator left out and why.
type Result struct {
	Edges   []Edge
	Skipped []string
}

type wiring struct {
	root    string
	edges   []Edge
	skipped []string
	modules map[string]string // a leaf's Go module path: the leaf
}

// Wiring reads the project umbrella at dir: umbrella.yaml, the packs of the realms it names, and in each
// leaf that is checked out go.mod, go.sum, package.json, package-lock.json, OpenAPI files and Go imports.
// A leaf that isn't checked out has the edges umbrella.yaml gives it, and no more.
func Wiring(dir string, packs map[string]*schema.Pack) (*Result, error) {
	u, err := schema.LoadUmbrella(dir)
	if err != nil {
		return nil, err
	}
	w := &wiring{root: dir, modules: map[string]string{}}
	var leaves []string
	for p, c := range u.Children {
		if c.Kind == schema.ChildLeaf {
			leaves = append(leaves, p)
		}
	}
	slices.Sort(leaves)
	for _, l := range leaves {
		c := u.Children[l]
		for _, q := range c.Consumes {
			w.add(l, "consumes", q, Extracted, schema.UmbrellaFile)
		}
		realm, ver := cmpOr(c.Realm, u.Realm), cmpOr(c.RealmVersion, u.RealmVersion)
		if realm != "" {
			w.add(l, "uses-realm", "realm:"+realm+"@"+ver, Extracted, schema.UmbrellaFile)
			if p := packs[realm]; p != nil {
				for _, s := range slices.Sorted(mapKeys(p.Services)) {
					w.add(l, "needs", "service:"+s, Extracted, "realm:"+realm+"/pack.yaml")
				}
			}
		}
		if m := w.goModule(l); m != "" {
			w.modules[m] = l
		}
	}
	for _, l := range leaves {
		w.goDeps(l)
		w.npmDeps(l)
	}
	return &Result{Edges: Sorted(w.edges), Skipped: slices.Sorted(slices.Values(w.skipped))}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// plain reports whether s may sit in a table cell: printable ASCII with none of | < > or a backtick.
func plain(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e || strings.ContainsRune("|<>`", r) {
			return false
		}
	}
	return true
}

func (w *wiring) skip(format string, a ...any) {
	w.skipped = append(w.skipped, fmt.Sprintf(format, a...))
}

func (w *wiring) add(from, kind, to, tag, source string) {
	if !plain(from) || !plain(to) || !plain(source) {
		w.skip("%s: a name that can't sit in the table: %q", source, to)
		return
	}
	w.edges = append(w.edges, Edge{From: from, To: to, Kind: kind, Tag: tag, Source: source})
}

// read returns the file at rel under the root: a regular file, not a link, within the size limit. A
// missing file is (nil, nil).
func (w *wiring) read(rel string) []byte {
	p := filepath.Join(w.root, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil || !fi.Mode().IsRegular():
		w.skip("%s: not a regular file", rel)
		return nil
	case fi.Size() > maxFile:
		w.skip("%s: over %d bytes", rel, maxFile)
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		w.skip("%s: %v", rel, err)
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(b) > maxFile {
		w.skip("%s: unreadable or over %d bytes", rel, maxFile)
		return nil
	}
	return b
}

// goMod parses what go.mod says: the module and its direct requirements.
func goMod(b []byte) (module string, req [][2]string) {
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		indirect := strings.Contains(line, "// indirect")
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "module":
			module = strings.Trim(f[1], `"`)
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && len(f) == 2 && !indirect:
			req = append(req, [2]string{f[0], f[1]})
		case !inBlock && len(f) == 3 && f[0] == "require" && !indirect:
			req = append(req, [2]string{f[1], f[2]})
		}
	}
	return module, req
}

func (w *wiring) goModule(leaf string) string {
	m, _ := goMod(w.read(leaf + "/go.mod"))
	return m
}

func (w *wiring) goDeps(leaf string) {
	b := w.read(leaf + "/go.mod")
	if b == nil {
		return
	}
	_, req := goMod(b)
	sum := string(w.read(leaf + "/go.sum"))
	for _, r := range req {
		if other, ok := w.modules[r[0]]; ok {
			w.add(leaf, "requires", other, Extracted, leaf+"/go.mod")
			continue
		}
		w.add(leaf, "requires", "go:"+r[0], Extracted, leaf+"/go.mod")
		if strings.Contains(sum, r[0]+" "+r[1]+" h1:") {
			w.add(leaf, "pins", "go:"+r[0]+"@"+r[1], Extracted, leaf+"/go.sum")
		}
	}
}

func (w *wiring) npmDeps(leaf string) {
	b := w.read(leaf + "/package.json")
	if b == nil {
		return
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		w.skip("%s/package.json: %v", leaf, err)
		return
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if lb := w.read(leaf + "/package-lock.json"); lb != nil {
		if err := json.Unmarshal(lb, &lock); err != nil {
			w.skip("%s/package-lock.json: %v", leaf, err)
		}
	}
	names := map[string]bool{}
	for n := range pkg.Dependencies {
		names[n] = true
	}
	for n := range pkg.DevDependencies {
		names[n] = true
	}
	for _, n := range slices.Sorted(mapKeys(names)) {
		if !plain(n) {
			w.skip("%s/package.json: a name that can't sit in the table: %q", leaf, n)
			continue
		}
		w.add(leaf, "requires", "npm:"+n, Extracted, leaf+"/package.json")
		if v := lock.Packages["node_modules/"+n].Version; v != "" {
			w.add(leaf, "pins", "npm:"+n+"@"+v, Extracted, leaf+"/package-lock.json")
		}
	}
}
