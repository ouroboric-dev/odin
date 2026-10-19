// Package shrink is the shrink guard (spec §8.3): a generated file committed at a pointer move,
// such as REPOS.md or a routing index, is refused when it has fewer than half of its previous
// version's rows. A buggy generator that empties an index then stops the move for a person,
// instead of merging the loss at every level.
package shrink

import (
	"bytes"
	"fmt"
	"path"

	"github.com/ouroboric-dev/odin/internal/routing"
)

// Error is a refused generated file. It stops the pointer move at the hard approval point
// shrink-guard.
type Error struct {
	Path     string
	Old, New int // rows before and after
}

func (e *Error) Error() string {
	return fmt.Sprintf("shrink guard: %s keeps %d of its %d rows, under half; a person decides whether the move goes ahead", e.Path, e.New, e.Old)
}

// Guard is Check for the generated files Odin knows: the wiring block of REPOS.md counts its edges, so the
// text a person wrote around it can't hide a loss, and .ygg/impact.json counts its edges, contracts and
// reach entries. A next version that has no block, or can't be read as an index, has no rows. Any other
// file counts its lines, as Check does. A previous version that can't be counted leaves nothing to
// compare with, so the next one passes.
func Guard(file string, prev, next []byte) error {
	var o, n int
	var ok bool
	switch {
	case path.Base(file) == routing.ReposFile:
		if o, ok = routing.WiringRows(prev); !ok {
			return Check(file, prev, next)
		}
		n, _ = routing.WiringRows(next)
	case file == routing.ImpactFile:
		if o, ok = routing.ImpactRows(prev); !ok {
			return nil
		}
		n, _ = routing.ImpactRows(next)
	default:
		return Check(file, prev, next)
	}
	if 2*n < o {
		return &Error{Path: file, Old: o, New: n}
	}
	return nil
}

// Rows counts a file's rows: its lines that aren't blank.
func Rows(b []byte) int {
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

// Check refuses next if it has fewer than half of prev's rows. A file that is new, or whose last
// version had no rows, always passes. A deleted file has no rows.
func Check(path string, prev, next []byte) error {
	o, n := Rows(prev), Rows(next)
	if 2*n < o {
		return &Error{Path: path, Old: o, New: n}
	}
	return nil
}
