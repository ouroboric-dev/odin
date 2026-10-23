package routing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/services"
	"go.yaml.in/yaml/v3"
)

// Limits on what the generator reads. A leaf is written by an agent, so the generator never follows a
// link out of it and never reads without a bound.
const (
	maxFile    = 1 << 20 // bytes of one file
	maxEntries = 20000   // files and folders walked in one leaf
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "testdata": true, "dist": true, ".factory": true}

// Result is a generated wiring: its edges, and what the generator left out and why.
type Result struct {
	Edges     []Edge
	Leaves    []string   // the leaves of the umbrella, sorted
	Contracts []Contract // the OpenAPI files found, sorted by path
	Skipped   []string
}

// Contract is an OpenAPI file that a leaf holds.
type Contract struct {
	Path   string `json:"path"` // from the project root
	Owner  string `json:"owner"`
	SHA256 string `json:"sha256"`
}

type wiring struct {
	root    string
	edges   []Edge
	skipped []string
	modules map[string]string // a leaf's Go module path: the leaf
}

type contractFile struct {
	leaf, rel, title string
	sum              string
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
		realm, ver := c.Realm, c.RealmVersion
		if realm != "" {
			w.add(l, "uses-realm", "realm:"+realm+"@"+ver, Extracted, schema.UmbrellaFile)
			if packs[realm] != nil {
				w.needs(u, packs, l, "realm:"+realm+"/pack.yaml")
			}
		}
		if m := w.goModule(l); m != "" {
			w.modules[m] = l
		}
	}
	var contracts []contractFile
	for _, l := range leaves {
		w.goDeps(l)
		w.npmDeps(l)
		w.goImports(l)
		contracts = append(contracts, w.contracts(l)...)
	}
	w.copies(contracts)
	w.mentions(leaves)
	res := &Result{Edges: Sorted(w.edges), Leaves: leaves, Skipped: slices.Sorted(slices.Values(w.skipped))}
	for _, c := range contracts {
		res.Contracts = append(res.Contracts, Contract{Path: c.rel, Owner: c.leaf, SHA256: c.sum})
	}
	slices.SortFunc(res.Contracts, func(a, b Contract) int { return strings.Compare(a.Path, b.Path) })
	return res, nil
}

// needs adds an edge from the leaf to each service its realm's sandbox starts, as services.For names them: the
// runtime realm's provider of each need of the leaf's realm, and the services the realm's pack names itself.
// The leaves it consumes are left out: their realms' needs are their own edges. A need with no provider gives
// no edge, and is reported.
func (w *wiring) needs(u *schema.Umbrella, packs map[string]*schema.Pack, leaf, source string) {
	svcs, err := services.For(nil, []string{leaf}, packLoader{u, packs})
	if err != nil {
		w.skip("%s: %v", source, err)
		return
	}
	for _, s := range svcs {
		w.add(leaf, "needs", "service:"+s.Name, Extracted, source)
	}
}

// packLoader is the services.Loader of the wiring: the packs of the realms the umbrella pins, by name.
type packLoader struct {
	u     *schema.Umbrella
	packs map[string]*schema.Pack
}

func (l packLoader) Leaf(repo string) (*realm.Realm, error) {
	c := l.u.Children[repo]
	if p := l.packs[c.Realm]; p != nil {
		return &realm.Realm{Name: c.Realm, Version: c.RealmVersion, Pack: p}, nil
	}
	return nil, fmt.Errorf("the realm %s of the leaf %s is not loaded", c.Realm, repo)
}

func (l packLoader) Runtime() (*realm.Realm, error) {
	if l.u.Runtime == "" {
		return nil, nil
	}
	if p := l.packs[l.u.Runtime]; p != nil {
		return &realm.Realm{Name: l.u.Runtime, Version: l.u.RuntimeVersion, Pack: p}, nil
	}
	return nil, fmt.Errorf("the runtime realm %s is not loaded", l.u.Runtime)
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

// walk lists the regular files of a leaf, by path relative to the root, never through a link.
func (w *wiring) walk(leaf string, keep func(rel string) bool) []string {
	var out []string
	n := 0
	base := filepath.Join(w.root, leaf)
	if fi, err := os.Lstat(base); err != nil || !fi.IsDir() {
		return nil
	}
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > maxEntries {
			w.skip("%s: more than %d entries; the rest is not read", leaf, maxEntries)
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(w.root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && skipDirs[d.Name()]:
			return filepath.SkipDir
		case d.Type()&fs.ModeSymlink != 0:
			if keep(rel) {
				w.skip("%s: a link, not followed", rel)
			}
		case d.Type().IsRegular() && keep(rel):
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func (w *wiring) goImports(leaf string) {
	if len(w.modules) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, rel := range w.walk(leaf, func(r string) bool { return strings.HasSuffix(r, ".go") && !strings.HasSuffix(r, "_test.go") }) {
		b := w.read(rel)
		if b == nil {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), rel, b, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for m, other := range w.modules {
				if other != leaf && !seen[other] && (p == m || strings.HasPrefix(p, m+"/")) {
					seen[other] = true
					w.add(leaf, "imports", other, Extracted, rel)
				}
			}
		}
	}
}

func isContractName(rel string) bool {
	b := path.Base(rel)
	return strings.HasSuffix(b, "openapi.yaml") || strings.HasSuffix(b, "openapi.yml")
}

func (w *wiring) contracts(leaf string) []contractFile {
	var out []contractFile
	for _, rel := range w.walk(leaf, isContractName) {
		b := w.read(rel)
		var doc struct {
			OpenAPI string `yaml:"openapi"`
			Info    struct {
				Title string `yaml:"title"`
			} `yaml:"info"`
		}
		if b == nil || yaml.Unmarshal(b, &doc) != nil || doc.OpenAPI == "" {
			if b != nil {
				w.skip("%s: not an OpenAPI document", rel)
			}
			continue
		}
		w.add(leaf, "holds", "contract:"+rel, Extracted, rel)
		out = append(out, contractFile{leaf: leaf, rel: rel, title: doc.Info.Title, sum: fmt.Sprintf("%x", sha256.Sum256(b))})
	}
	return out
}

// copies links a leaf's contract to the contract of a leaf it depends on, when both name the same API.
// Identical bytes are a fact (extracted). The same title with other bytes is a guess that it is the
// same contract gone out of date (inferred).
func (w *wiring) copies(cs []contractFile) {
	linked := map[[2]string]bool{}
	for _, e := range w.edges {
		if e.Tag == Extracted && (e.Kind == "consumes" || e.Kind == "requires" || e.Kind == "imports") {
			linked[[2]string{e.From, e.To}] = true
		}
	}
	for _, c := range cs {
		for _, o := range cs {
			if c.leaf == o.leaf || c.title == "" || c.title != o.title || !linked[[2]string{c.leaf, o.leaf}] {
				continue
			}
			tag := Inferred
			if c.sum == o.sum {
				tag = Extracted
			}
			w.add("contract:"+c.rel, "copies", "contract:"+o.rel, tag, c.rel)
		}
	}
}

// mentions is a guess: a leaf's README or AGENTS.md names another leaf. It is added only where no
// extracted edge already joins the two.
func (w *wiring) mentions(leaves []string) {
	joined := map[[2]string]bool{}
	for _, e := range w.edges {
		if e.Tag == Extracted {
			joined[[2]string{e.From, e.To}] = true
		}
	}
	for _, l := range leaves {
		for _, doc := range []string{"README.md", "AGENTS.md"} {
			b := w.read(l + "/" + doc)
			if b == nil {
				continue
			}
			for _, o := range leaves {
				if o == l || joined[[2]string{l, o}] {
					continue
				}
				if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(o) + `\b`).Match(b) {
					w.add(l, "mentions", o, Inferred, l+"/"+doc)
					joined[[2]string{l, o}] = true
				}
			}
		}
	}
}
