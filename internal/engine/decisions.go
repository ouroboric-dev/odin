package engine

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/ouroboric-dev/odin/internal/run"
)

// The operator's gate decisions reach a session as its decisions input, a ygg.decisions/v1 document (decisionsOf,
// in inputs.go). Until a gate takes a decisions file (typed-T19), the run holds each approval's note, kept here.

// checkNote refuses a note that is not one line: a note with a newline, a carriage return, a tab or any
// other control character, or a line or paragraph separator (U+2028, U+2029), could start a heading of
// its own in a prompt. It is the rule that
// ygg-proof-run.sh applies to --note and --reason.
func checkNote(note string) error {
	breaks := func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) }
	if strings.IndexFunc(note, breaks) >= 0 {
		return fmt.Errorf("the note has a control character or a line or paragraph separator; a note is one line of text")
	}
	return nil
}

// keepDecision keeps note as the product owner's decision at gate, when it is not blank. It replaces the
// note an earlier approval of the same gate gave: a back_to return past the gate (land or move back to
// implement) meets it again, and its latest note holds. A blank note restates nothing, so the earlier
// decision still holds.
func keepDecision(s *run.State, gate, note string) {
	if note = strings.TrimSpace(note); note != "" {
		s.Decisions = slices.DeleteFunc(s.Decisions, func(d run.Decision) bool { return d.Gate == gate })
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
