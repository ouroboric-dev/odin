// Package sandboxfake is a stand-in for the devcontainer sandbox, for tests of Odin that must not
// need a container engine. It "starts" nothing, and runs commands on the host in the leaf's folder,
// so a test sees the files and processes a real sandbox would give it, and can count what went
// through the sandbox.
package sandboxfake

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ouroboric-dev/gleipnir/scan"
	"github.com/ouroboric-dev/gungnir"
)

// Sandbox implements gungnir.Sandbox on the host.
type Sandbox struct {
	mu    sync.Mutex
	Ups   []gungnir.SandboxSpec
	Downs []gungnir.Env
	Execs []gungnir.Cmd // every command but the secret scan
	Scans int           // how many secret scans ran
	// ScanFiles is the files each secret scan was given, in order.
	ScanFiles [][]string
	// UpErr, if set, is what Up returns.
	UpErr error
	// Scan is how the secret scan ends. The zero value is a clean tree. Exit 7 with a gitleaks
	// report in Report means secrets were found.
	Scan struct {
		Exit   int
		Report string
	}
	// OnScan, if set, sees each secret scan, with the files it was given, before the scan ends: a test
	// reads what the scan would read, or changes the files after it, as a real scan's timing allows.
	OnScan func(env gungnir.Env, c gungnir.Cmd, files []string)
	// Fail, if set, is asked before every command but the secret scan. An error it returns is what Exec
	// returns, as the real driver does when podman itself fails.
	Fail func(gungnir.Cmd) error
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
	hook, result, fail, onScan := s.Intercept, s.Scan, s.Fail, s.OnScan
	isScan := slices.Equal(c.Argv, scan.Argv()) // the secret scan: gitleaks isn't on the host
	var files []string
	if isScan {
		s.Scans++
		if c.Stdin != nil {
			b, _ := io.ReadAll(c.Stdin)
			files = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
			if len(b) == 0 {
				files = []string{}
			}
		}
		s.ScanFiles = append(s.ScanFiles, files)
	} else {
		s.Execs = append(s.Execs, c)
	}
	s.mu.Unlock()
	if isScan {
		if onScan != nil {
			onScan(env, c, files)
		}
		c.Stdout.Write([]byte(result.Report))
		return gungnir.ExecResult{ExitCode: result.Exit}, nil
	}
	if fail != nil {
		if err := fail(c); err != nil {
			return gungnir.ExecResult{}, err
		}
	}
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
	if ctx.Err() != nil { // like the real driver: a command stopped by its context is the context's error
		return gungnir.ExecResult{}, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return gungnir.ExecResult{ExitCode: ee.ExitCode()}, nil
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
