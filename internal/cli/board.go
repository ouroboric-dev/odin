package cli

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/tracker"
)

// board runs ygg board: the project's tasks by column (spec §10.3), read from features.json and the run it
// names. With --all it shows the board of every project in the operator's local list, and it shows nothing if
// any path of the list is not a project. It writes nothing.
func (c *command) board(args []string) int {
	var all bool
	var list string
	f, _, ok := c.parse("board", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.BoolVar(&all, "all", false, "show every project of the local list")
		fs.StringVar(&list, "projects", "", "the list of projects (default "+tracker.DefaultProjectsFile()+")")
	})
	if !ok {
		return ExitRefused
	}
	if all {
		if f.set["project"] {
			return c.refuse(fmt.Errorf("--all shows the projects of the local list; it takes no --project"))
		}
		if list == "" {
			list = tracker.DefaultProjectsFile()
		}
		projects, err := tracker.LoadProjects(list)
		if err != nil {
			return c.refuse(err)
		}
		var boards []gungnir.Board
		for _, p := range projects {
			b, err := tracker.Local{Project: p}.Board(c.ctx)
			if err != nil {
				return c.refuse(fmt.Errorf("%s: %w", p, err))
			}
			boards = append(boards, b)
		}
		tracker.WriteAll(c.out, boards)
		return ExitDone
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
