package run

import (
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

// A publish of a node that declares no output names that, not an empty contract with no view.
func TestAPublishOfANodeWithNoOutputSaysSo(t *testing.T) {
	nodes := []schema.Node{{ID: "a", Kind: schema.KindExec}, {ID: "a-docs", Kind: schema.KindPublish, From: "a", To: "docs"}}
	err := checkPublish("graph.yaml", nodes, map[string]schema.Node{})
	if err == nil || err.Error() != "graph.yaml: node a-docs: publish names a, which declares no output" {
		t.Errorf("checkPublish() = %v", err)
	}
	if err := checkPublish("graph.yaml", nodes[1:], map[string]schema.Node{}); err == nil || !strings.Contains(err.Error(), "publish names a, which declares no output") {
		t.Errorf("checkPublish() of an unknown node = %v", err)
	}
}

// A tasks node's planner must take a rejection, at the top of a graph and in a foreach body alike.
func TestATasksNodeWhosePlannerTakesNoRejectionIsRefused(t *testing.T) {
	plan := schema.Node{ID: "plan", Kind: schema.KindAgent, Input: []string{"refusal"}, Output: string(schema.ContractPlan)}
	ready := schema.Node{ID: "ready", Kind: schema.KindTasks, From: "plan"}
	want := "graph.yaml: node ready: tasks reads plan, which must declare rejection: a blocked plan is sent back to it with a person's note"
	if err := checkPublish("graph.yaml", []schema.Node{plan, ready}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() = %v; want %q", err, want)
	}
	if err := checkPublish("graph.yaml", []schema.Node{plan, {ID: "each", Kind: schema.KindForeach, Body: []schema.Node{ready}}}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() of a body = %v; want %q", err, want)
	}
	plan.Input = []string{"rejection", "refusal"}
	if err := checkPublish("graph.yaml", []schema.Node{plan, ready}, map[string]schema.Node{}); err != nil {
		t.Errorf("checkPublish() of a planner that takes a rejection = %v", err)
	}
}

// A loop's reviewer must take a rejection: a person's answer to its block is sent to it (ruling R52). At the top of a
// graph and in a foreach body alike.
func TestALoopReviewerThatTakesNoRejectionIsRefused(t *testing.T) {
	loop := schema.Node{ID: "build", Kind: schema.KindLoop, Input: []string{"rejection"}, Output: string(schema.ContractImplement),
		Reviewers: []schema.Reviewer{{ID: "spec", Input: []string{"change"}, Output: string(schema.ContractReview)}}}
	want := "graph.yaml: node build: reviewer spec must declare rejection: a person's answer to its block is sent to it"
	if err := checkPublish("graph.yaml", []schema.Node{loop}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() = %v; want %q", err, want)
	}
	if err := checkPublish("graph.yaml", []schema.Node{{ID: "each", Kind: schema.KindForeach, Body: []schema.Node{loop}}}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() of a body = %v; want %q", err, want)
	}
	loop.Reviewers[0].Input = []string{"change", "rejection"}
	if err := checkPublish("graph.yaml", []schema.Node{loop}, map[string]schema.Node{}); err != nil {
		t.Errorf("checkPublish() of a reviewer that takes a rejection = %v", err)
	}
}

// A loop must take a rejection: a person's rejection of its escalation is sent to its implementer.
func TestALoopThatTakesNoRejectionIsRefused(t *testing.T) {
	loop := schema.Node{ID: "build", Kind: schema.KindLoop, Input: []string{"refusal"}, Output: string(schema.ContractImplement)}
	want := "graph.yaml: node build: a loop must declare rejection: a person's rejection of its escalation is sent to its implementer"
	if err := checkPublish("graph.yaml", []schema.Node{loop}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() = %v; want %q", err, want)
	}
	if err := checkPublish("graph.yaml", []schema.Node{{ID: "each", Kind: schema.KindForeach, Body: []schema.Node{loop}}}, map[string]schema.Node{}); err == nil || err.Error() != want {
		t.Errorf("checkPublish() of a body = %v; want %q", err, want)
	}
}
