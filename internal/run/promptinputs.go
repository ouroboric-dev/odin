package run

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
)

// CheckRealmPrompts checks the implementer prompts that realms give to the loops of g. A loop with no prompt
// of its own takes its implementer's from the realm of its leaf (pack.yaml, leaf.implementer), which gungnir's
// CheckPrompts cannot read. Which realm a loop in a foreach body gets is decided when its leaf is known, so
// every realm in realms that has an implementer prompt is held to every such loop: the prompt may name, in
// backticks, only the inputs the loop declares (change typed-boundaries, ruling R66).
func CheckRealmPrompts(g *schema.Graph, realms []*realm.Realm) error {
	var outputs []string
	var loops []schema.Node
	var walk func(nodes []schema.Node)
	walk = func(nodes []schema.Node) {
		for _, n := range nodes {
			if n.Output != "" {
				outputs = append(outputs, n.ID)
			}
			if n.Kind == schema.KindLoop && n.Prompt == "" {
				loops = append(loops, n)
			}
			walk(n.Body)
		}
	}
	walk(g.Nodes)
	rs := slices.Clone(realms)
	slices.SortFunc(rs, func(a, b *realm.Realm) int { return strings.Compare(a.Name, b.Name) })
	for _, r := range rs {
		if r.Pack.Leaf == nil || r.Pack.Leaf.Implementer == "" {
			continue
		}
		path := filepath.Join(r.Dir, r.Pack.Leaf.Implementer)
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("realm %s: %w", r.Name, err)
		}
		for _, n := range loops {
			for _, in := range schema.PromptInputs(string(b), outputs) {
				if !slices.Contains(n.Input, in) {
					return fmt.Errorf("realm %s: the implementer prompt %s (%s) names the input %s, which loop %s does not declare; a session sees only the inputs its node declares",
						r.Name, r.Pack.Leaf.Implementer, path, in, n.ID)
				}
			}
		}
	}
	return nil
}
