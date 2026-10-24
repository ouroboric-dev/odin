package engine

import (
	"strings"

	"github.com/ouroboric-dev/gungnir"
)

// conformanceSection is what the gate file of a spec gate says about the verdict.
func conformanceSection(c gungnir.Conformance) string {
	var b strings.Builder
	b.WriteString("## Design conformance\n\n")
	if c.Blocked != nil {
		b.WriteString("The reviewer could not check the spec against the design, and says why (written by an agent):\n\n")
		for _, line := range strings.Split(strings.TrimSpace(c.Blocked.Reason), "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
		return b.String()
	}
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
