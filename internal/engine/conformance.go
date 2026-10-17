package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// maxVerdictBytes is the most a design-conformance verdict may hold: a deviation and its amendment.
const maxVerdictBytes = 64 << 10

// readConformance reads the verdict that the scratch reviewer node wrote to out/conformance.json (spec
// §4.2): through the checks every file of an agent gets (a regular file inside the workspace, bounded),
// and then exactly (schema.ParseConformance). A verdict that is missing or can't be read is an error,
// never a pass: it would let a deviation through the spec gate (spec §4.6).
func readConformance(r *run.Run, node string) (gungnir.Conformance, error) {
	dir := filepath.Join(r.ScratchDir(node), "out")
	st, err := os.Lstat(filepath.Join(dir, "conformance.json"))
	if err != nil {
		return gungnir.Conformance{}, fmt.Errorf("%s wrote no out/conformance.json", node)
	}
	data, err := readRegular(dir, "conformance.json", st, maxVerdictBytes)
	if err != nil {
		return gungnir.Conformance{}, fmt.Errorf("out/conformance.json of %s can't be read: %w", node, err)
	}
	c, err := schema.ParseConformance(data)
	if err != nil {
		return gungnir.Conformance{}, fmt.Errorf("out/conformance.json of %s: %w", node, err)
	}
	return c, nil
}

// isVerdictSource reports whether a gate of the graph reads the verdict of the scratch node id.
func isVerdictSource(r *run.Run, id string) bool {
	return slices.ContainsFunc(r.Nodes(), func(m schema.Node) bool { return m.Kind == schema.KindGate && m.DeviationFrom == id })
}

// conformanceSection is what the gate file of a spec gate says about the verdict.
func conformanceSection(c gungnir.Conformance) string {
	var b strings.Builder
	b.WriteString("## Design conformance\n\n")
	if !c.Deviation {
		b.WriteString("The spec does not deviate from the design.\n\n")
		return b.String()
	}
	b.WriteString("The spec deviates from the design. The reviewer proposes this amendment (a suggestion, written by an agent):\n\n")
	for _, line := range strings.Split(strings.TrimSpace(c.Amendment), "\n") {
		b.WriteString("> " + line + "\n")
	}
	b.WriteString("\n")
	return b.String()
}
