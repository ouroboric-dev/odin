package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every command of the usage text is in the README, and the architecture page names the commands and the
// node of the grader and the routing and dependency work. A new command with no line in the README fails here.
func TestTheDocsNameEveryCommand(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	arch, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	commands := regexp.MustCompile(`(?m)^  (ygg [a-z]+(?: bump| build)?)`).FindAllStringSubmatch(usage, -1)
	if len(commands) < 9 {
		t.Fatalf("found %d commands in the usage text, want at least 9", len(commands))
	}
	for _, m := range commands {
		if !strings.Contains(string(readme), m[1]) {
			t.Errorf("README.md does not show %q", m[1])
		}
	}
	for _, want := range []string{"ygg grade", "ygg deps bump", "`grade` node", "--canary", "--host", "--task", "grade_refused", "score.json", "ygg.grader",
		"REPOS.md", ".ygg/impact.json", "shrink guard", "pointer-contract", "routing_index_built", "pointer_contract_checked", "deps_bump", "memory: none",
		"ygg metrics", "ygg.factory.version", "dispute_ignored", "approval wait", "--graph", "graphs.dir", "graph pin", "ygg.run/v3", "factory layer", "snapshot/<node>/<realm>"} {
		if !strings.Contains(string(arch), want) {
			t.Errorf("docs/architecture.md does not mention %q", want)
		}
	}
}

// The architecture page says what the dollar caps count.
func TestTheDocsSayWhatTheDollarCapsCount(t *testing.T) {
	arch, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arch), "Usage.BilledUSD") {
		t.Error("docs/architecture.md does not mention Usage.BilledUSD")
	}
}
