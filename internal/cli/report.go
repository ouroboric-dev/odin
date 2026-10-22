package cli

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/report"
	"github.com/ouroboric-dev/odin/internal/run"
)

// report runs ygg report: it writes report.md into the run's folder (spec §10.3). With --commit it also commits
// a copy as docs/runs/<run-id>.md, the one file, because a project ignores the run folder.
func (c *command) report(args []string) int {
	var commit bool
	f, pos, ok := c.parse("report", args, 1, func(fs *flag.FlagSet, f *flags) {
		fs.BoolVar(&commit, "commit", false, "also commit a copy as docs/runs/<run-id>.md")
	})
	if !ok {
		return ExitRefused
	}
	project, err := filepath.Abs(f.project)
	if err != nil {
		return c.refuse(err)
	}
	ids, err := runIDs(project, pos[0])
	if err != nil || len(ids) != 1 || ids[0] != pos[0] {
		if err == nil {
			err = errNotOneRun(pos[0])
		}
		return c.refuse(err)
	}
	dir := run.Dir(project, pos[0])
	m, err := metrics.Collect(dir)
	if err != nil {
		return c.refuse(err)
	}
	st, err := run.LoadState(dir)
	if err != nil {
		return c.refuse(err)
	}
	var b bytes.Buffer
	if err := report.Write(&b, m, st); err != nil {
		return c.refuse(err)
	}
	path := filepath.Join(dir, "report.md")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		return c.refuse(err)
	}
	fmt.Fprintf(c.out, "report: %s\n", path)
	if !commit {
		return ExitDone
	}
	rel := filepath.Join("docs", "runs", pos[0]+".md")
	if err := os.MkdirAll(filepath.Join(project, "docs", "runs"), 0o755); err != nil {
		return c.refuse(err)
	}
	if err := os.WriteFile(filepath.Join(project, rel), b.Bytes(), 0o644); err != nil {
		return c.refuse(err)
	}
	if _, err := git.Run(c.ctx, project, "add", "--", rel); err != nil {
		return c.refuse(err)
	}
	if _, err := git.Run(c.ctx, project, "commit", "-q", "-m", "Report of run "+pos[0], "--", rel); err != nil {
		return c.refuse(err)
	}
	fmt.Fprintf(c.out, "committed: %s\n", rel)
	return ExitDone
}
