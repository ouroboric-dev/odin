package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// hostRunner runs a command on the host, in dir, in a process group of its own. It is the runner of
// Odin's own generators in an umbrella, and of every command of a run without a sandbox (only
// scripted agents run there). The command gets PATH, HOME, which is the run's own directory so that
// caches never land in a repo, and LANG, and nothing else. While it runs, run.json records its process
// group, so a ygg that is killed leaves a node the next resume or ygg doctor can stop.
type hostRunner struct {
	r    *run.Run
	node string
	dir  string
}

var _ gungnir.Runner = hostRunner{}

func (h hostRunner) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	if err := c.Validate(); err != nil {
		return gungnir.ExecResult{}, err
	}
	if len(c.Env) > 0 {
		return gungnir.ExecResult{}, errors.New("a command on the host takes no environment of its own: credentials belong to the sandbox")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = filepath.Join(h.dir, c.Dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.r.Home(), "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		// A program that can't start is a command that failed, as in a shell: status 127.
		fmt.Fprintf(c.Stderr, "ygg: %v\n", err)
		return gungnir.ExecResult{ExitCode: 127}, nil
	}
	pid := cmd.Process.Pid
	h.r.State.Exec = &run.ExecProcess{Node: h.node, PGID: pid, Start: run.ProcStart(pid)}
	if err := h.r.Save(); err != nil {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
		return gungnir.ExecResult{}, err
	}
	err := cmd.Wait()
	h.r.State.Exec = nil
	if ctx.Err() != nil {
		return gungnir.ExecResult{}, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return gungnir.ExecResult{}, nil
	case errors.As(err, &ee):
		return gungnir.ExecResult{ExitCode: ee.ExitCode()}, nil
	}
	return gungnir.ExecResult{}, err
}
