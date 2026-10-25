// Package git runs host git for Odin (spec §3.3). Every call disables hooks and fsmonitor, ignores
// the operator's global and system git config and GIT_* variables, allows only the https, ssh and
// file transports, and commits as Odin. Leaves are always run with an explicit git dir, so a leaf's
// own .git file, which an agent can rewrite, is never trusted.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Odin's identity on every commit it makes.
const (
	Name  = "Yggdrasil"
	Email = "ygg@localhost"
)

// safe are the settings every host git call gets. -c settings reach the git processes git starts
// itself, such as a submodule clone.
var safe = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.excludesFile=/dev/null", // git reads $XDG_CONFIG_HOME/git/ignore even without a global config
	"-c", "core.attributesFile=/dev/null", // and $XDG_CONFIG_HOME/git/attributes
	"-c", "protocol.allow=never",
	"-c", "protocol.https.allow=always",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.file.allow=always",
	"-c", "submodule.recurse=false",
	"-c", "commit.gpgsign=false",
	"-c", "tag.gpgsign=false",
	"-c", "init.defaultBranch=main",
	"-c", "user.name=" + Name,
	"-c", "user.email=" + Email,
}

// env is the operator's environment without any GIT_* variable, plus Odin's own.
func env() []string {
	var e []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			e = append(e, kv)
		}
	}
	return append(e,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+Name, "GIT_AUTHOR_EMAIL="+Email,
		"GIT_COMMITTER_NAME="+Name, "GIT_COMMITTER_EMAIL="+Email,
		"LC_ALL=C",
	)
}

// Run runs git with args in dir and returns its standard output, trimmed. An error carries the
// command and git's own message.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Output(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// Output is Run with git's standard output as it is, for output a trim would change, such as a
// -z list whose first name starts with a space.
func Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	cmd.Env = env()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// Repo is a repo whose git dir Odin chose itself.
type Repo struct {
	GitDir   string // the repo's git dir, such as <parent>/.git/modules/<name>
	WorkTree string // its working copy
}

// Run runs git with args against r, with the git dir and work tree given explicitly.
func (r Repo) Run(ctx context.Context, args ...string) (string, error) {
	return Run(ctx, r.WorkTree, append([]string{"--git-dir=" + r.GitDir, "--work-tree=" + r.WorkTree}, args...)...)
}

// Output is Run with git's standard output untrimmed.
func (r Repo) Output(ctx context.Context, args ...string) ([]byte, error) {
	return Output(ctx, r.WorkTree, append([]string{"--git-dir=" + r.GitDir, "--work-tree=" + r.WorkTree}, args...)...)
}

// maxExclude bounds what Odin reads of a repo's info/exclude: a few rules of its own.
const maxExclude = 64 << 10

// Exclude adds pattern, one line of gitignore, to the exclude file of r's git dir, once. The git dir is Odin's,
// outside the working copy, and a sandbox mounts a leaf's read-only, so a session can't take the rule back; git
// add, git status and git clean, as Odin runs them, all keep to it.
func (r Repo) Exclude(pattern string) error {
	dir := filepath.Join(r.GitDir, "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, "exclude")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxExclude+1))
	if err != nil {
		return err
	}
	if len(b) > maxExclude {
		return fmt.Errorf("%s is larger than Odin writes it", p)
	}
	if slices.Contains(strings.Split(string(b), "\n"), pattern) {
		return nil
	}
	line := pattern + "\n"
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		line = "\n" + line
	}
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return f.Close()
}

// Pipe runs git with args against r and hands its standard output to read as it is produced, for output
// too big to hold, such as an archive of the repo. read decides how much it takes: if it returns early,
// git is stopped. An error of git's wins over one of read's only when read saw the end of the output.
func (r Repo) Pipe(ctx context.Context, read func(io.Reader) error, args ...string) error {
	return r.PipeIn(ctx, nil, read, args...)
}

// PipeIn is Pipe with stdin given to git as its standard input, for commands such as cat-file --batch.
func (r Repo) PipeIn(ctx context.Context, stdin io.Reader, read func(io.Reader) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	full := append(append([]string{}, safe...), "--git-dir="+r.GitDir, "--work-tree="+r.WorkTree)
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Dir = r.WorkTree
	cmd.Env = env()
	cmd.Stdin = stdin
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readErr := read(out)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	switch {
	case readErr != nil:
		return readErr
	case waitErr != nil:
		return fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), r.WorkTree, waitErr, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// CheckURL refuses a repo URL that host git shouldn't fetch: anything but https, ssh (also in the
// git@host:path form) and local paths, and anything git could read as an option.
func CheckURL(url string) error {
	switch {
	case url == "":
		return errors.New("an empty repo URL")
	case strings.HasPrefix(url, "-"):
		return fmt.Errorf("repo URL %q starts with -, so git would read it as an option", url)
	case strings.Contains(url, "::"):
		return fmt.Errorf("repo URL %q uses a git remote helper (::)", url)
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "ssh://"),
		strings.HasPrefix(url, "/"), strings.HasPrefix(url, "./"), strings.HasPrefix(url, "../"):
		return nil
	case !strings.Contains(url, "://") && strings.Contains(url, "@") && strings.Contains(url, ":"):
		return nil // git@host:owner/repo
	}
	return fmt.Errorf("repo URL %q is not https, ssh or a local path", url)
}
