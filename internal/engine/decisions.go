package engine

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/ouroboric-dev/odin/internal/run"
)

// decisionsHeading heads the block of the notes that gate approvals gave (spec §7.2).
const decisionsHeading = "## The product owner's decisions"

// decisionsBlock is the text every session after an approval gets, after the brief: each note kept so
// far, in the order the approvals were given, with its gate. It is "" when no note was kept, so an
// approval with no note adds nothing.
func decisionsBlock(s *run.State) string {
	if len(s.Decisions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(decisionsHeading + "\n\nDecisions the product owner gave at the approval points of this run. " +
		"They hold for this product: where one differs from an earlier document, the decision wins, " +
		"and the difference is written down as the product owner's decision.\n\n")
	for _, d := range s.Decisions {
		fmt.Fprintf(&b, "- %s: %s\n", d.Gate, d.Note)
	}
	return b.String()
}

// withDecisions adds the block of decisions to text, after what is there, when there is one.
func withDecisions(text string, s *run.State) string {
	if block := decisionsBlock(s); block != "" {
		return text + "\n\n" + block
	}
	return text
}

// checkNote refuses a note that is not one line: a note with a newline, a carriage return, a tab or any
// other control character could start a heading of its own in a prompt. It is the rule that
// ygg-proof-run.sh applies to --note and --reason.
func checkNote(note string) error {
	if strings.IndexFunc(note, unicode.IsControl) >= 0 {
		return fmt.Errorf("the note has a control character; a note is one line of text")
	}
	return nil
}

// keepDecision keeps note as the product owner's decision at gate, when it is not blank.
func keepDecision(s *run.State, gate, note string) {
	if note = strings.TrimSpace(note); note != "" {
		s.Decisions = append(s.Decisions, run.Decision{Gate: gate, Note: note})
	}
}

// dropDecisions drops the notes of the gates with these ids: the run meets them again.
func dropDecisions(s *run.State, gates map[string]bool) {
	kept := s.Decisions[:0]
	for _, d := range s.Decisions {
		if !gates[d.Gate] {
			kept = append(kept, d)
		}
	}
	s.Decisions = kept
}
