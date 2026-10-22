package cli

import (
	"path/filepath"

	"github.com/ouroboric-dev/odin/internal/tracker"
)

// board runs ygg board: the project's tasks by column (spec §10.3), read from features.json and the run it
// names. It writes nothing.
func (c *command) board(args []string) int {
	f, _, ok := c.parse("board", args, 0, nil)
	if !ok {
		return ExitRefused
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	b, err := tracker.Local{Project: project}.Board(c.ctx)
	if err != nil {
		return c.refuse(err)
	}
	tracker.WriteBoard(c.out, b)
	return ExitDone
}
