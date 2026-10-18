package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheBlockReplacesItselfAndKeepsThePeopleText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ReposFile)
	human := "# Repos\n\nWhat each repo is for.\n\n"
	os.WriteFile(p, []byte(human), 0o644)
	one := []Edge{{From: "a", To: "b", Kind: "consumes", Tag: Extracted, Source: "umbrella.yaml"}}
	two := append(one, Edge{From: "b", To: "c", Kind: "consumes", Tag: Inferred, Source: "b/README.md"})
	for _, es := range [][]Edge{one, two, two} {
		if err := WriteWiring(dir, es); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p)
	want := human + string(Block(two))
	if string(got) != want {
		t.Errorf("REPOS.md =\n%s\nwant\n%s", got, want)
	}
	os.WriteFile(p, []byte("top\n"+string(Block(one))+"\nbottom\n"), 0o644)
	WriteWiring(dir, two)
	got, _ = os.ReadFile(p)
	if !strings.HasPrefix(string(got), "top\n"+BeginMarker) || !strings.HasSuffix(string(got), EndMarker+"\n\nbottom\n") {
		t.Errorf("text around the block changed:\n%s", got)
	}
}

func TestAStrayMarkerIsAnErrorAndCutsNothing(t *testing.T) {
	for name, doc := range map[string]string{
		"two begins":   BeginMarker + "\nx\n" + BeginMarker + "\n" + EndMarker + "\n",
		"end first":    EndMarker + "\n" + BeginMarker + "\n",
		"only a begin": "text\n" + BeginMarker + "\nmore text\n",
	} {
		if out, err := ReplaceBlock([]byte(doc), Block(nil)); err == nil {
			t.Errorf("%s: no error, got %q", name, out)
		}
	}
}
