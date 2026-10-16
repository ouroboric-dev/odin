// Package sandboxfake is a stand-in for the devcontainer sandbox, for tests of Odin that must not
// need a container engine. It "starts" nothing, and runs commands on the host in the leaf's folder,
// so a test sees the files and processes a real sandbox would give it, and can count what went
// through the sandbox.
package sandboxfake

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/ouroboric-dev/gungnir"
)

// Sandbox implements gungnir.Sandbox on the host.
type Sandbox struct {
	mu    sync.Mutex
	Ups   []gungnir.SandboxSpec
	Downs []gungnir.Env
	Execs []gungnir.Cmd
	// UpErr, if set, is what Up returns.
	UpErr error
	// Intercept, if set, sees every command first. A true handled result is the command's result.
	Intercept func(gungnir.Cmd) (res gungnir.ExecResult, handled bool)
}

var _ gungnir.Sandbox = (*Sandbox)(nil)

func (s *Sandbox) Up(_ context.Context, spec gungnir.SandboxSpec) (gungnir.Env, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.UpErr != nil {
		return gungnir.Env{}, s.UpErr
	}
	s.Ups = append(s.Ups, spec)
	return gungnir.Env{Name: spec.Name, Project: "fake-" + spec.Run + "-" + filepath.Base(spec.Leaf), Container: "fake-" + filepath.Base(spec.Leaf), Workdir: spec.Leaf}, nil
}

// Exec runs the command with only PATH, and the variables the command names, in the leaf.
func (s *Sandbox) Exec(ctx context.Context, env gungnir.Env, c gungnir.Cmd) (gungnir.ExecResult, error) {
	if err := c.Validate(); err != nil {
		return gungnir.ExecResult{}, err
	}
	s.mu.Lock()
	s.Execs = append(s.Execs, c)
	hook := s.Intercept
	s.mu.Unlock()
	if hook != nil {
		if res, ok := hook(c); ok {
			return res, nil
		}
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = filepath.Join(env.Workdir, c.Dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "LANG=C.UTF-8"}
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return gungnir.ExecResult{ExitCode: ee.ExitCode()}, nil
	}
	if ctx.Err() != nil {
		return gungnir.ExecResult{}, ctx.Err()
	}
	return gungnir.ExecResult{}, err
}

func (s *Sandbox) Down(_ context.Context, env gungnir.Env) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Downs = append(s.Downs, env)
	return nil
}

// ExecCount returns how many commands ran in the sandbox.
func (s *Sandbox) ExecCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Execs)
}
