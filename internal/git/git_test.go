package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

// repo makes a repo with one commit and returns its directory.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	if _, err := Run(ctx, dir, "add", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, dir, "commit", "-q", "-m", "first"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hook writes an executable hook script that leaves marker behind if it ever runs.
func hook(t *testing.T, path, marker string) {
	t.Helper()
	write(t, path, "#!/bin/sh\ntouch "+marker+"\n")
}

func TestHooksNeverRun(t *testing.T) {
	dir := repo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), marker)
	hook(t, filepath.Join(dir, "myhooks", "pre-commit"), marker)
	if _, err := Run(ctx, dir, "config", "core.hooksPath", "myhooks"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "b.txt"), "b\n")
	if _, err := Run(ctx, dir, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, dir, "commit", "-q", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook ran during a host commit")
	}
}

func TestFsmonitorNeverRuns(t *testing.T) {
	dir := repo(t)
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "fsmonitor-ran")
	hook(t, filepath.Join(tmp, "monitor"), marker)
	if _, err := Run(ctx, dir, "config", "core.fsmonitor", filepath.Join(tmp, "monitor")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, dir, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("core.fsmonitor ran a command during git status")
	}
}

func TestTheOperatorsGitSettingsNeverApply(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook(t, filepath.Join(home, "hooks", "pre-commit"), marker)
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\thooksPath = "+filepath.Join(home, "hooks")+
		"\n[commit]\n\tgpgsign = true\n[user]\n\tname = Operator\n[status]\n\tshowUntrackedFiles = no\n")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("GIT_AUTHOR_NAME", "Somebody Else")
	dir := repo(t) // would fail on gpgsign with no key, or on GIT_DIR, if either applied
	if _, err := os.Stat(marker); err == nil {
		t.Error("the operator's hooksPath ran a hook")
	}
	if who, _ := Run(ctx, dir, "log", "-1", "--format=%an <%ae>|%cn"); who != Name+" <"+Email+">|"+Name {
		t.Errorf("commit author|committer = %q, want Odin's identity", who)
	}
	write(t, filepath.Join(dir, "new.txt"), "new\n")
	if st, _ := Run(ctx, dir, "status", "--porcelain"); !strings.Contains(st, "new.txt") {
		t.Errorf("git status hides an untracked file (%q): the operator's status.showUntrackedFiles applied", st)
	}
}

func TestRepoNeverTrustsTheWorkTreesGitFile(t *testing.T) {
	dir := repo(t)
	real := Repo{GitDir: filepath.Join(dir, ".git"), WorkTree: dir}
	moved := filepath.Join(t.TempDir(), "gitdir")
	if err := os.Rename(real.GitDir, moved); err != nil {
		t.Fatal(err)
	}
	real.GitDir = moved
	// The work tree's .git now points at a repo an agent could have planted, with a hook.
	evil := repo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	write(t, filepath.Join(evil, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n\thooksPath = "+filepath.Join(evil, "h")+"\n")
	hook(t, filepath.Join(evil, "h", "pre-commit"), marker)
	write(t, filepath.Join(dir, ".git"), "gitdir: "+filepath.Join(evil, ".git")+"\n")
	write(t, filepath.Join(dir, "c.txt"), "c\n")
	if _, err := real.Run(ctx, "add", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := real.Run(ctx, "commit", "-q", "-m", "third"); err != nil {
		t.Fatal(err)
	}
	if n, _ := real.Run(ctx, "rev-list", "--count", "HEAD"); n != "2" {
		t.Errorf("the real repo has %s commits, want 2", n)
	}
	if n, _ := Run(ctx, evil, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("the planted repo has %s commits, want 1 (untouched)", n)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the planted repo's hook ran")
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"https://github.com/ouroboric-dev/odin.git", "ssh://git@github.com/o/r.git",
		"git@github.com:o/r.git", "/srv/repos/r.git", "./repos/api", "../site"} {
		if err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"ext::sh -c touch% /tmp/pwned", "fd::17", "-u./payload", "--upload-pack=touch /tmp/x",
		"http://example.com/r.git", "", "git://example.com/r.git"} {
		if err := CheckURL(bad); err == nil {
			t.Errorf("CheckURL(%q) = nil, want a refusal", bad)
		}
	}
}

func TestErrorsCarryGitsMessage(t *testing.T) {
	_, err := Run(ctx, t.TempDir(), "log")
	if err == nil || !strings.Contains(err.Error(), "git log") || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("Run() error = %v, want the command and git's message", err)
	}
}
