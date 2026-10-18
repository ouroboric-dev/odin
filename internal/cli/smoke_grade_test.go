package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

// The grader smoke (v0-H16) runs this realm on a real engine, with ouroboric/scripts/ygg-smoke-grade.sh; this
// test keeps it loadable: a loop, then the merge and the pointer, then the grade node last, with the prompts
// and the template it names, and a sandbox stack pinned by digest.
func TestTheGradeSmokeRealmLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "smoke-grade")
	pack, err := schema.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := schema.LoadGraph(filepath.Join(dir, pack.Graph))
	if err != nil {
		t.Fatal(err)
	}
	first, last := g.Nodes[0], g.Nodes[len(g.Nodes)-1]
	if first.Kind != schema.KindLoop || last.Kind != schema.KindGrade || len(first.Reviewers) != 1 {
		t.Errorf("the graph starts with %+v and ends with %+v", first, last)
	}
	if len(pack.Checks["unit"]) == 0 || pack.Sandbox == nil || pack.Sandbox.Stack == "" {
		t.Errorf("the pack: checks %v, sandbox %+v", pack.Checks, pack.Sandbox)
	}
	for _, f := range []string{first.Prompt, first.Reviewers[0].Prompt, "templates/notes/LICENSE"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("the realm names %s: %v", f, err)
		}
	}
}

// An agent that is told it is graded can work for the grade. The prompts of the realm never say so.
func TestTheGradeSmokePromptsNeverMentionTheGrading(t *testing.T) {
	words := regexp.MustCompile(`(?i)grader|grade|grading|benchmark|hidden|score`)
	prompts, err := filepath.Glob(filepath.Join("..", "..", "testdata", "smoke-grade", "prompts", "*.md"))
	if err != nil || len(prompts) != 2 {
		t.Fatalf("prompts = %v, %v", prompts, err)
	}
	for _, p := range prompts {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if w := words.Find(b); w != nil {
			t.Errorf("%s mentions %q", p, w)
		}
	}
}
