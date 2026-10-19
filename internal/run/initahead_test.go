package run

import (
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

func TestAnInitNodeIsAheadOnlyAtOrAfterTheRunsPosition(t *testing.T) {
	s := &Setup{Graph: &schema.Graph{Nodes: []schema.Node{
		{ID: "a", Kind: schema.KindGate}, {ID: "leaves", Kind: schema.KindInit}, {ID: "b", Kind: schema.KindGate}}}}
	cases := []struct {
		name string
		st   *State
		want bool
	}{
		{"a new run", nil, true},
		{"a resume before init", &State{Position: 0}, true},
		{"a resume at init, which failed", &State{Position: 1}, true},
		{"a resume past init", &State{Position: 2}, false},
	}
	for _, c := range cases {
		if got := s.initAhead(c.st); got != c.want {
			t.Errorf("%s: initAhead() = %v, want %v", c.name, got, c.want)
		}
	}
	if (&Setup{Graph: &schema.Graph{Nodes: []schema.Node{{ID: "a", Kind: schema.KindGate}}}}).initAhead(nil) {
		t.Error("a graph with no init node has one ahead")
	}
}
