package realm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// fixture writes a realm named name at version into dir.
func fixture(t *testing.T, dir, name, version string) {
	t.Helper()
	write(t, dir, "pack.yaml", "name: "+name+"\nversion: "+version+"\ngraph: graph.yaml\n", 0o644)
	write(t, dir, "graph.yaml", "name: "+name+"\nversion: "+version+"\nnodes:\n  - { id: a, kind: exec, run: [true], repo: . }\n", 0o644)
	write(t, dir, "prompts/implementer.md", "Build it.\n", 0o644)
}

func TestLoadReadsTheRealmAndHashesIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "midgard-react")
	fixture(t, dir, "midgard-react", "0.1.0")
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if r.Name != "midgard-react" || r.Version != "0.1.0" || r.Dir != dir || r.Pack.Graph != "graph.yaml" || len(r.Hash) != 64 {
		t.Errorf("Load() = %+v", r)
	}
	again, _ := Load(dir)
	if again.Hash != r.Hash {
		t.Error("the hash of an unchanged realm changed")
	}
}

func TestTheHashFollowsEveryFileTheRunCouldRead(t *testing.T) {
	base := t.TempDir()
	hashOf := func(mutate func(dir string)) string {
		dir := filepath.Join(base, "r"+strings.ReplaceAll(t.Name(), "/", "-"))
		os.RemoveAll(dir)
		fixture(t, dir, "midgard-react", "0.1.0")
		mutate(dir)
		h, err := Hash(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	plain := hashOf(func(string) {})
	for name, mutate := range map[string]func(string){
		"a prompt changes":        func(d string) { write(t, d, "prompts/implementer.md", "Build it twice.\n", 0o644) },
		"a file is added":         func(d string) { write(t, d, "prompts/extra.md", "x", 0o644) },
		"a file is removed":       func(d string) { os.Remove(filepath.Join(d, "prompts/implementer.md")) },
		"a file becomes a script": func(d string) { os.Chmod(filepath.Join(d, "prompts/implementer.md"), 0o755) },
	} {
		if got := hashOf(mutate); got == plain {
			t.Errorf("%s, and the hash did not change", name)
		}
	}
	if got := hashOf(func(d string) { write(t, d, ".git/HEAD", "ref: x", 0o644) }); got != plain {
		t.Error("the hash follows .git, which holds nothing a run reads")
	}
}

// A realm's files are its own: a symlink anywhere in it is refused when it is loaded, before anything is
// read. Prompts, first-commit files and the template are read by following links, so a link could read
// outside the realm, and a change at its target would not change the realm's hash (review M-7).
func TestARealmWithASymlinkInItIsRefused(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret.md", "outside the realm\n", 0o644)
	write(t, outside, "pack.yaml", "name: midgard-react\nversion: 0.1.0\ngraph: graph.yaml\n", 0o644)
	for name, c := range map[string]struct{ link, target string }{
		"a prompt that leads out":      {"prompts/link.md", filepath.Join(outside, "secret.md")},
		"a prompt that stays inside":   {"prompts/link.md", "implementer.md"},
		"a folder that leads out":      {"templates", outside},
		"the pack itself, leading out": {"pack.yaml", filepath.Join(outside, "pack.yaml")},
	} {
		dir := filepath.Join(t.TempDir(), "midgard-react")
		fixture(t, dir, "midgard-react", "0.1.0")
		os.Remove(filepath.Join(dir, c.link))
		if err := os.Symlink(c.target, filepath.Join(dir, c.link)); err != nil {
			t.Fatal(err)
		}
		if r, err := Load(dir); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, c.link)) || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: Load() = %+v, %v; want a refusal naming %s as a symlink", name, r, err, c.link)
		}
		if _, err := Hash(dir); err == nil {
			t.Errorf("%s: Hash() = nil error", name)
		}
	}
}

// A realm whose root folder is a symlink is refused too: the hash walk does not enter a linked root, so
// its hash would be that of an empty folder and a resume would accept any change (review I-1).
func TestARealmWhoseRootIsASymlinkIsRefused(t *testing.T) {
	real := filepath.Join(t.TempDir(), "midgard-react")
	fixture(t, real, "midgard-react", "0.1.0")
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if r, err := Load(link); err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Load(linked root) = %+v, %v; want a refusal naming %s as a symlink", r, err, link)
	}
	if r, err := Load(real); err != nil || r.Name != "midgard-react" {
		t.Errorf("Load(normal realm) = %+v, %v", r, err)
	}
}

func TestTheHashRefusesARealmTooBigToBeOne(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "midgard-react", "0.1.0")
	for i := 0; i < 5; i++ {
		write(t, dir, "f"+string(rune('a'+i)), "x", 0o644)
	}
	if _, err := hashLimited(dir, limits{files: 4, bytes: 1 << 20}); err == nil || !strings.Contains(err.Error(), "files") {
		t.Errorf("too many files: %v", err)
	}
	if _, err := hashLimited(dir, limits{files: 100, bytes: 20}); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("too many bytes: %v", err)
	}
}

func TestAStoreFindsARealmByName(t *testing.T) {
	store := t.TempDir()
	fixture(t, filepath.Join(store, "midgard-react"), "midgard-react", "0.1.0")
	fixture(t, filepath.Join(store, "helheim-go"), "midgard-react", "0.1.0") // a folder holding another realm than its name says
	s := &Store{Dir: store}
	if dir, err := s.Find("midgard-react"); err != nil || dir != filepath.Join(store, "midgard-react") {
		t.Errorf("Find() = %q, %v", dir, err)
	}
	for name, want := range map[string]string{"helheim-go": "holds the realm midgard-react", "asgard": "no realm asgard", "../x": "not a realm name", "Midgard": "not a realm name"} {
		if _, err := s.Find(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Find(%q) = %v, want an error containing %q", name, err, want)
		}
	}
}

func TestResolveTakesAPathOrAName(t *testing.T) {
	store := t.TempDir()
	fixture(t, filepath.Join(store, "midgard-react"), "midgard-react", "0.1.0")
	s := &Store{Dir: store}
	if dir, byName, err := Resolve(filepath.Join(store, "midgard-react"), s); err != nil || byName || dir != filepath.Join(store, "midgard-react") {
		t.Errorf("a path: %q, %v, %v", dir, byName, err)
	}
	if dir, byName, err := Resolve("midgard-react", s); err != nil || !byName || dir != filepath.Join(store, "midgard-react") {
		t.Errorf("a name: %q, %v, %v", dir, byName, err)
	}
	if _, _, err := Resolve("midgard-react", nil); err == nil || !strings.Contains(err.Error(), "realms") {
		t.Errorf("a name with no store: %v", err)
	}
	if dir, byName, err := Resolve("./testdata/x", nil); err != nil || byName || dir != "./testdata/x" {
		t.Errorf("a relative path with no store: %q, %v, %v", dir, byName, err)
	}
}
