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
	"os"
	"os/exec"
	"strings"
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
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	cmd.Env = env()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
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
