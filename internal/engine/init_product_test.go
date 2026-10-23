package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

const realmStart = "#!/bin/sh\necho the realm's start\n"

// withProduct is the runtime realm rf naming a product start, with the script in its folder when body is not "".
func withProduct(rf realmFiles, body string) realmFiles {
	rf.pack += "product:\n  start: start.sh\n"
	if body != "" {
		rf.files = map[string]string{"start.sh": body}
	}
	return rf
}

// commitFile writes a file of the project and commits it.
func commitFile(t *testing.T, project, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(project, name), mode); err != nil {
		t.Fatal(err)
	}
	git1(t, project, "add", "--", name)
	git1(t, project, "commit", "-q", "-m", "the project's "+name, "--", name)
}

func TestInitWritesProductStart(t *testing.T) {
	ctx := context.Background()
	runtime := runtimeFiles("niflheim-local", "0.1.0", "postgres", "s3")
	realms := append(galleryRealms(), withProduct(runtime, realmStart))
	leaves := runtimeLine("niflheim-local", "0.1.0") + galleryLeaves
	node := schema.Node{ID: "leaves", Kind: schema.KindInit, From: "docs/leaves.yaml"}

	r := initRun(t, leaves, realms...)
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	path := filepath.Join(r.Project, "start.sh")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != realmStart {
		t.Fatalf("start.sh = %q, %v; want the realm's file", got, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
		t.Fatalf("start.sh mode = %v; want 0755", st.Mode().Perm())
	}
	if mode := git1(t, r.Project, "ls-tree", "HEAD", "--", "start.sh"); !strings.HasPrefix(mode, "100755 ") {
		t.Errorf("start.sh is committed as %q; want mode 100755", mode)
	}
	if files := git1(t, r.Project, "log", "--format=%s", "--name-only", "-1", "--", "start.sh"); !strings.Contains(files, "start.sh") || strings.Count(files, "\n") > 3 {
		t.Errorf("start.sh is not in a commit of its own: %q", files)
	}
	if ig, _ := os.ReadFile(filepath.Join(r.Project, ".gitignore")); !strings.Contains(string(ig), "\n.data/\n") && !strings.HasPrefix(string(ig), ".data/\n") {
		t.Errorf(".gitignore = %q; want .data/", ig)
	}
	if st := git1(t, r.Project, "status", "--porcelain"); strings.Contains(st, "start.sh") || strings.Contains(st, ".gitignore") {
		t.Errorf("start.sh or .gitignore is left uncommitted: %q", st)
	}

	// Run again, init adds no commit.
	head := git1(t, r.Project, "rev-parse", "HEAD")
	if err := runInit(ctx, r, node); err != nil {
		t.Fatalf("init run again = %v", err)
	}
	if again := git1(t, r.Project, "rev-parse", "HEAD"); again != head {
		t.Errorf("init run again moved the project from %s to %s", head, again)
	}

	// A start.sh that differs gets the realm's, in a new commit that says so.
	commitFile(t, r.Project, "start.sh", "#!/bin/sh\necho mine\n", 0o755)
	if err := runInit(ctx, r, node); err != nil {
		t.Fatalf("init with another start.sh = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != realmStart {
		t.Errorf("start.sh = %q; want the realm's", b)
	}
	if msg := git1(t, r.Project, "log", "-1", "--format=%s"); !strings.Contains(msg, "replace start.sh") {
		t.Errorf("the new commit says %q", msg)
	}
	// So does one that has the content and lost the mode.
	commitFile(t, r.Project, "start.sh", realmStart, 0o644)
	if err := runInit(ctx, r, node); err != nil {
		t.Fatalf("init with a start.sh that is not executable = %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
		t.Errorf("start.sh mode = %v; want 0755", st.Mode().Perm())
	}

	// With no runtime realm, or a runtime realm with no product start, the project's own start.sh stays.
	for _, c := range []struct {
		name, leaves string
		realms       []realmFiles
	}{
		{"no runtime realm", galleryLeaves, nil},
		{"no product start", leaves, append(galleryRealms(), runtime)},
	} {
		name := c.name
		own := initRun(t, c.leaves, c.realms...)
		commitFile(t, own.Project, "start.sh", "#!/bin/sh\necho the smoke's\n", 0o644)
		before := git1(t, own.Project, "rev-parse", "HEAD")
		if out, err := Advance(ctx, own); err != nil || out.Status != run.StatusDone {
			t.Fatalf("%s: Advance() = %+v, %v", name, out, err)
		}
		if b, _ := os.ReadFile(filepath.Join(own.Project, "start.sh")); string(b) != "#!/bin/sh\necho the smoke's\n" {
			t.Errorf("%s: start.sh = %q; want the project's own", b, name)
		}
		if log := git1(t, own.Project, "log", "--format=%s", before+"..HEAD", "--", "start.sh", ".gitignore"); strings.Contains(log, "start.sh") || strings.Contains(log, ".data/") {
			t.Errorf("%s: init touched start.sh or ignored .data/: %q", name, log)
		}
	}

	// A runtime realm whose start script is missing makes init fail before it creates a leaf.
	missing := initRun(t, leaves, append(galleryRealms(), withProduct(runtime, ""))...)
	out, err := Advance(ctx, missing)
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "start.sh") {
		t.Fatalf("a runtime realm with no start script: Advance() = %+v, %v; want a failed run naming start.sh", out, err)
	}
	if u, _ := schema.LoadUmbrella(missing.Project); len(u.Children) != 0 {
		t.Errorf("a leaf was created: %v", u.Paths())
	}
	if _, err := os.Stat(filepath.Join(missing.Project, "start.sh")); err == nil {
		t.Error("start.sh was written")
	}
}
