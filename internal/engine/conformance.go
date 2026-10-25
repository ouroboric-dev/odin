package engine

import (
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
)

// conformanceSection is what the gate file of a spec gate says about the verdict. The reviewer's blocked reason and
// amendment are its words: each reads as its text (mdsafe).
func conformanceSection(c gungnir.Conformance) string {
	var b strings.Builder
	b.WriteString("## Design conformance\n\n")
	if c.Blocked != nil {
		b.WriteString("The reviewer could not check the spec against the design, and says why (written by an agent):\n\n")
		b.WriteString(mdsafe.Text(strings.TrimSpace(c.Blocked.Reason)) + "\n\n")
		return b.String()
	}
	if !c.Deviation {
		b.WriteString("The spec does not deviate from the design.\n\n")
		return b.String()
	}
	b.WriteString("The spec deviates from the design. The reviewer proposes this amendment (a suggestion, written by an agent):\n\n")
	b.WriteString(mdsafe.Text(strings.TrimSpace(c.Amendment)) + "\n\n")
	return b.String()
}
