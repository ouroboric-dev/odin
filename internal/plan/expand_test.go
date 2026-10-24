package plan

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

const shape = `
name: g
version: 1.0.0
schema: 2
nodes:
  - { id: before, kind: exec, run: [echo, before], repo: . }
  - id: tasks
    kind: foreach
    over: tasks
    nodes:
      - id: implement
        kind: loop
        input: [rejection, refusal]
        output: ygg.implement/v1
        realm: leaf
        role: implementer
        reviewers:
          - { id: spec, role: reviewer, prompt: prompts/review-spec.md, output: ygg.review/v1 }
      - { id: feature-gate, kind: gate, gate: feature }
      - { id: land, kind: merge, realm: leaf, back_to: implement }
      - { id: merge-gate, kind: gate, gate: merge }
      - { id: move, kind: pointer, realm: leaf, acceptance: [e2e], back_to: implement }
  - { id: after, kind: exec, run: [echo, after], repo: . }
`

func graph(t *testing.T) *schema.Graph {
	t.Helper()
	g, err := schema.ParseGraph("graph.yaml", []byte(shape))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func recipes(layer string) (Recipe, error) {
	switch layer {
	case "repos/api":
		return Recipe{Realm: "helheim-go", Implementer: "prompts/implementer-api.md", Checks: []string{"vet", "unit"}}, nil
	case "repos/web":
		return Recipe{Realm: "midgard-react", Implementer: "prompts/implementer-web.md", Checks: []string{"lint", "unit", "e2e"}}, nil
	}
	return Recipe{}, fmt.Errorf("the project has no leaf %s", layer)
}

var tasks = []schema.Feature{{ID: "gallery-T001", Layer: "repos/api"}, {ID: "gallery-T002", Layer: "repos/web", DependsOn: []string{"gallery-T001"}}}

func TestEveryTaskGetsTheBodyInItsLeafAndTheRealmOfThatLeaf(t *testing.T) {
	g := graph(t)
	nodes, err := Expand(g.Nodes[1], tasks, g, recipes)
	if err != nil {
		t.Fatalf("Expand() = %v", err)
	}
	var ids []string
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	want := "gallery-t001-implement gallery-t001-feature-gate gallery-t001-land gallery-t001-merge-gate gallery-t001-move " +
		"gallery-t002-implement gallery-t002-feature-gate gallery-t002-land gallery-t002-merge-gate gallery-t002-move"
	if strings.Join(ids, " ") != want {
		t.Fatalf("ids =\n%s\nwant\n%s", strings.Join(ids, " "), want)
	}
	api, web := nodes[0], nodes[5]
	if api.Task != "gallery-t001" || api.Repo != "repos/api" || api.Realm != "helheim-go" || api.Prompt != "prompts/implementer-api.md" ||
		!slices.Equal(api.Checks, []string{"vet", "unit"}) || api.Role != "implementer" || api.Attempts != schema.DefaultAttempts {
		t.Errorf("the api's loop = %+v", api)
	}
	if web.Realm != "midgard-react" || web.Prompt != "prompts/implementer-web.md" || !slices.Equal(web.Checks, []string{"lint", "unit", "e2e"}) || web.Repo != "repos/web" {
		t.Errorf("the web's loop = %+v", web)
	}
	if rv := api.Reviewers[0]; rv.ID != "spec" || rv.Prompt != "prompts/review-spec.md" || api.SessionKey(rv) != "gallery-t001-implement-spec" {
		t.Errorf("the reviewer = %+v, key %s", rv, api.SessionKey(rv))
	}
	gate, land, move := nodes[1], nodes[2], nodes[4]
	if gate.Reruns != "gallery-t001-implement" || gate.Task != "gallery-t001" || gate.Kind != schema.KindGate {
		t.Errorf("the feature gate = %+v", gate)
	}
	if land.BackTo != "gallery-t001-implement" || land.Returns != schema.DefaultReturns || land.Repo != "repos/api" ||
		!slices.Equal(land.Checks, []string{"vet", "unit"}) || land.Realm != "helheim-go" {
		t.Errorf("the merge = %+v", land)
	}
	if move.BackTo != "gallery-t001-implement" || !slices.Equal(move.Acceptance, []string{"e2e"}) || !slices.Equal(move.Checks, []string{"vet", "unit"}) {
		t.Errorf("the pointer move = %+v", move)
	}
	if nodes[8].Reruns != "gallery-t002-implement" {
		t.Errorf("the second task's merge gate reruns %q", nodes[8].Reruns)
	}
}

func TestExpandingChangesNothingInTheGraph(t *testing.T) {
	g := graph(t)
	before := fmt.Sprintf("%+v", g.Nodes[1])
	if _, err := Expand(g.Nodes[1], tasks, g, recipes); err != nil {
		t.Fatal(err)
	}
	if after := fmt.Sprintf("%+v", g.Nodes[1]); after != before {
		t.Errorf("Expand changed the foreach node:\n%s\n%s", before, after)
	}
	a, _ := Expand(g.Nodes[1], tasks[:1], g, recipes)
	a[0].Checks[0] = "changed"
	b, _ := Expand(g.Nodes[1], tasks[:1], g, recipes)
	if b[0].Checks[0] != "vet" {
		t.Error("two expansions share one checks slice")
	}
}

func TestExpandRefusesWhatItCannotPlace(t *testing.T) {
	g := graph(t)
	if _, err := Expand(g.Nodes[1], []schema.Feature{{ID: "x-T1", Layer: "repos/db"}}, g, recipes); err == nil || !strings.Contains(err.Error(), "x-T1") || !strings.Contains(err.Error(), "repos/db") {
		t.Errorf("a task in a leaf nobody knows: %v", err)
	}
	none := func(string) (Recipe, error) { return Recipe{Realm: "notes"}, nil }
	if _, err := Expand(g.Nodes[1], tasks, g, none); err == nil || !strings.Contains(err.Error(), "offers no leaf recipe") {
		t.Errorf("a realm with no recipe: %v", err)
	}
	// A node id the graph already uses.
	clash := graph(t)
	clash.Nodes = append(clash.Nodes, schema.Node{ID: "gallery-t001-land", Kind: schema.KindExec})
	if _, err := Expand(clash.Nodes[1], tasks, clash, recipes); err == nil || !strings.Contains(err.Error(), "gallery-t001-land") {
		t.Errorf("an id the graph uses: %v", err)
	}
	keyClash := graph(t)
	keyClash.Nodes = append(keyClash.Nodes, schema.Node{ID: "gallery-t001-implement-spec", Kind: schema.KindExec})
	if _, err := Expand(keyClash.Nodes[1], tasks, keyClash, recipes); err == nil || !strings.Contains(err.Error(), "gallery-t001-implement-spec") {
		t.Errorf("a reviewer's session key the graph uses as a node id: %v", err)
	}
	if got, err := Expand(g.Nodes[1], nil, g, recipes); err != nil || len(got) != 0 {
		t.Errorf("no tasks = %v, %v", got, err)
	}
}

// An input that names a body node is that task's copy of it, as a re-run and a back_to are: its output is kept
// under the copy's id. An input that names a node before the foreach, or a built-in source, stays as it is.
func TestAnInputOfAnEarlierBodyNodeNamesTheTasksOwnCopy(t *testing.T) {
	body := strings.Replace(shape, "      - { id: feature-gate", "      - { id: sum, kind: agent, role: planner, prompt: p.md, input: [task, implement, before-plan, rejection], output: ygg.implement/v1 }\n      - { id: feature-gate", 1)
	body = strings.Replace(body, "  - { id: before, kind: exec, run: [echo, before], repo: . }\n",
		"  - { id: before, kind: exec, run: [echo, before], repo: . }\n  - { id: before-plan, kind: agent, role: planner, prompt: p.md, input: [brief], output: ygg.plan/v1 }\n", 1)
	body = strings.Replace(body, "          - { id: spec, role: reviewer, prompt: prompts/review-spec.md, output: ygg.review/v1 }",
		"          - { id: spec, role: reviewer, prompt: prompts/review-spec.md, input: [task, change], output: ygg.review/v1 }", 1)
	g, err := schema.ParseGraph("graph.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := Expand(g.Nodes[2], tasks[:1], g, recipes)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(nodes, func(n schema.Node) bool { return n.ID == "gallery-t001-sum" })
	if i < 0 {
		t.Fatalf("no sum node in %v", nodes)
	}
	if want := []string{"task", "gallery-t001-implement", "before-plan", "rejection"}; !slices.Equal(nodes[i].Input, want) {
		t.Errorf("sum's input = %v, want %v", nodes[i].Input, want)
	}
	if got := nodes[0].Reviewers[0].Input; !slices.Equal(got, []string{"task", "change"}) {
		t.Errorf("the reviewer's input = %v", got)
	}
	if g.Nodes[2].Body[1].Input[1] != "implement" {
		t.Error("Expand changed the graph's own node")
	}
}
