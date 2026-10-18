package shrink

import (
	"fmt"

	"errors"
	"github.com/ouroboric-dev/odin/internal/routing"
	"strings"
	"testing"
)

func rows(n int) []byte { return []byte(strings.Repeat("| row |\n", n)) }

func TestShrinkGuard(t *testing.T) {
	cases := []struct {
		name     string
		old, new []byte
		refused  bool
	}{
		{"a new file", nil, rows(3), false},
		{"growth", rows(10), rows(12), false},
		{"exactly half", rows(10), rows(5), false},
		{"under half", rows(10), rows(4), true},
		{"emptied", rows(10), []byte("\n\n"), true},
		{"deleted", rows(10), nil, true},
		{"blank lines aren't rows", []byte("a\n\n\nb\n\n"), []byte("a\n"), false},
		{"an old file with no rows", []byte("\n"), nil, false},
	}
	for _, c := range cases {
		err := Check("REPOS.md", c.old, c.new)
		var se *Error
		if c.refused != errors.As(err, &se) {
			t.Errorf("%s: Check() = %v, refused want %v", c.name, err, c.refused)
		}
	}
	err := Check("REPOS.md", rows(10), rows(4))
	if err == nil || !strings.Contains(err.Error(), "REPOS.md") || !strings.Contains(err.Error(), "4 of its 10 rows") {
		t.Errorf("the refusal doesn't say what shrank: %v", err)
	}
}

// block makes a REPOS.md with a wiring block of n edges and some text a person wrote around it.
func block(n int, prose int) []byte {
	var es []routing.Edge
	for i := range n {
		es = append(es, routing.Edge{From: fmt.Sprintf("leaf%03d", i), To: "api", Kind: "consumes", Tag: routing.Extracted, Source: "umbrella.yaml"})
	}
	return append([]byte(strings.Repeat("What a repo is for.\n", prose)+"\n"), routing.Block(es)...)
}

// Review focus: the guard counts the index's edges, so a person's text around the block can't hide a loss.
func TestGuardCountsTheWiringRowsNotTheProse(t *testing.T) {
	prev := block(10, 40)
	if err := Check("REPOS.md", prev, block(2, 40)); err != nil {
		t.Fatalf("Check() counts lines, so it should miss this loss: %v", err)
	}
	var se *Error
	if err := Guard("REPOS.md", prev, block(2, 40)); !errors.As(err, &se) || se.Old != 10 || se.New != 2 {
		t.Errorf("Guard() = %v; want a refusal that keeps 2 of 10 edges", err)
	}
	for name, next := range map[string][]byte{
		"exactly half": block(5, 40), "growth": block(30, 1), "a first block": block(3, 0),
	} {
		old := prev
		if name == "a first block" {
			old = []byte("# Repos\n")
		}
		if err := Guard("REPOS.md", old, next); err != nil {
			t.Errorf("%s: Guard() = %v", name, err)
		}
	}
	if err := Guard("REPOS.md", prev, []byte(strings.Repeat("prose\n", 50))); err == nil {
		t.Error("a REPOS.md that lost its block is not refused")
	}
}

func impactJSON(t *testing.T, edges int) []byte {
	t.Helper()
	b, err := (&routing.Impact{Schema: routing.ImpactSchema, Bound: "lower", Edges: edges, Extracted: edges,
		Contracts: []routing.ContractEntry{}, InferredEdges: []routing.Edge{}, // the exact schema wants the lists present
		Reach: map[string]routing.Reached{"api": {Extracted: []string{}, Inferred: []string{}}}}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGuardRefusesAnImpactIndexThatLostEdges(t *testing.T) {
	prev := impactJSON(t, 20)
	var se *Error
	if err := Guard(routing.ImpactFile, prev, impactJSON(t, 3)); !errors.As(err, &se) || se.Path != routing.ImpactFile {
		t.Errorf("Guard() = %v; want a refusal", err)
	}
	if err := Guard(routing.ImpactFile, prev, []byte("{not json")); err == nil {
		t.Error("an index that can't be read is not refused")
	}
	if err := Guard(routing.ImpactFile, prev, impactJSON(t, 10)); err != nil {
		t.Errorf("half is fine: %v", err)
	}
	if err := Guard(routing.ImpactFile, nil, impactJSON(t, 1)); err != nil {
		t.Errorf("a first index is fine: %v", err)
	}
	if err := Guard(routing.ImpactFile, []byte("{damaged"), impactJSON(t, 1)); err != nil {
		t.Errorf("a damaged old index leaves nothing to compare: %v", err)
	}
	if err := Guard("other.md", rows(10), rows(4)); err == nil {
		t.Error("other files keep the row rule")
	}
}
