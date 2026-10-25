package run

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

// A session sees only the inputs its node declares, so a prompt that works from "the brief" needs `brief` in
// its node's input. The real-agent smoke found a pack whose prompts said "the brief below" with no brief
// declared; scripted agents never read prompts, so no other test caught it. This walks every graph pack in
// testdata and holds each agent, loop and reviewer session to it.
func TestEveryTestPackSessionThatNamesTheBriefDeclaresIt(t *testing.T) {
	root := filepath.Join("..", "..", "testdata")
	var graphs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "graph.yaml" {
			graphs = append(graphs, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, file := range graphs {
		g, err := schema.LoadGraph(file)
		if err != nil {
			continue // not a graph pack that loads on its own
		}
		dir := filepath.Dir(file)
		if err := g.CheckPrompts(dir); err != nil {
			t.Errorf("%s: %v", file, err)
		}
		check := func(node, prompt string, input []string) {
			if prompt == "" {
				return
			}
			b, err := os.ReadFile(filepath.Join(dir, prompt))
			if err != nil {
				return
			}
			checked++
			if strings.Contains(strings.ToLower(string(b)), "brief") && !slices.Contains(input, "brief") {
				t.Errorf("%s: %s: the prompt %s names the brief, but input %v does not declare it", file, node, prompt, input)
			}
			// The rule of the prompts (R66) reads backticks: a prompt that works from the brief names it as `brief`.
			if strings.Contains(strings.ToLower(string(b)), "brief") && !slices.Contains(schema.PromptInputs(string(b), nil), "brief") {
				t.Errorf("%s: %s: the prompt %s says brief, but does not name the input in backticks", file, node, prompt)
			}
		}
		for _, n := range g.All() {
			if n.Kind == schema.KindAgent || n.Kind == schema.KindLoop {
				check(n.ID, n.Prompt, n.Input)
			}
			for _, r := range n.Reviewers {
				check(n.ID+"/"+r.ID, r.Prompt, r.Input)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no session was checked: the walk found no graph pack")
	}
}
