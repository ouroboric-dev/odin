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
