package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

func TestYggReportWritesReportMDInTheRunFolderAndWritesItTheSameWayTwice(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	code, out, errOut := ygg(t, "report", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	path := filepath.Join(run.Dir(dir, id), "report.md")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report.md was not written: %v", err)
	}
	if !strings.Contains(out, path) || !strings.HasPrefix(string(first), "# Run "+id) || !strings.Contains(string(first), "## Blocked tasks") {
		t.Errorf("stdout %q, report.md:\n%s", out, first)
	}
	if code, _, _ = ygg(t, "report", id, "--project", dir); code != ExitDone {
		t.Fatal("a second report failed")
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Error("two reports of one run differ")
	}
}

func TestYggReportCommitCopiesTheReportIntoTheProjectAndCommitsOnlyIt(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("not mine"), 0o644)
	if _, err := git.Run(context.Background(), dir, "add", "unrelated.txt"); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := ygg(t, "report", id, "--commit", "--project", dir); code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	copy, err := os.ReadFile(filepath.Join(dir, "docs", "runs", id+".md"))
	if err != nil || !strings.HasPrefix(string(copy), "# Run "+id) {
		t.Fatalf("docs/runs/%s.md: %v", id, err)
	}
	subject, _ := git.Run(context.Background(), dir, "log", "-1", "--format=%s", "HEAD")
	if !strings.HasPrefix(subject, "Report of run "+id) {
		t.Errorf("the last commit is %q", subject)
	}
	files, _ := git.Run(context.Background(), dir, "show", "--name-only", "--format=", "HEAD")
	if strings.TrimSpace(files) != "docs/runs/"+id+".md" {
		t.Errorf("the last commit holds %q; want only docs/runs/%s.md", files, id)
	}
	staged, _ := git.Run(context.Background(), dir, "diff", "--cached", "--name-only")
	if strings.TrimSpace(staged) != "unrelated.txt" {
		t.Errorf("staged after the command: %q; want unrelated.txt still staged and not committed", staged)
	}
}

func TestYggReportRefusesWhatItCannotTrust(t *testing.T) {
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ := walk(t, dir, config)
	for name, args := range map[string][]string{
		"no id": {"report", "--project", dir}, "a path": {"report", "../x", "--project", dir},
		"a range": {"report", id + ".." + id, "--project", dir}, "a run that is not there": {"report", "20200101-000000-aaaaaa", "--project", dir},
	} {
		if code, _, errOut := ygg(t, args...); code != ExitRefused || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
}
