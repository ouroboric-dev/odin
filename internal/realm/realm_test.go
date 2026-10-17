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
		"a symlink appears":       func(d string) { os.Symlink("../pack.yaml", filepath.Join(d, "prompts/link.md")) },
	} {
		if got := hashOf(mutate); got == plain {
			t.Errorf("%s, and the hash did not change", name)
		}
	}
	if got := hashOf(func(d string) { write(t, d, ".git/HEAD", "ref: x", 0o644) }); got != plain {
		t.Error("the hash follows .git, which holds nothing a run reads")
	}
	a := hashOf(func(d string) { os.Symlink("../pack.yaml", filepath.Join(d, "prompts/link.md")) })
	b := hashOf(func(d string) { os.Symlink("../graph.yaml", filepath.Join(d, "prompts/link.md")) })
	if a == b {
		t.Error("two symlinks with different targets hash alike")
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
