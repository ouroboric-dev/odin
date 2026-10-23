package run

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"context"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// fakeReleaser stands in for the real one: it makes, for each leaf, what Make writes into the leaf's folder.
type fakeReleaser struct {
	Calls  [][]string
	Seen   []string // the files of the snapshot at the last call
	Make   func(t *testing.T, leafDir string)
	Err    error
	Output string // what the build prints
	ID     string // its Key
	t      *testing.T
	Parent string
}

func (f *fakeReleaser) Key() string { return f.ID }

func (f *fakeReleaser) Build(_ context.Context, spec ReleaseSpec) (string, error) {
	leaves := spec.Leaves
	f.Calls = append(f.Calls, slices.Clone(leaves))
	f.Seen = filesOf(f.t, spec.Snapshot)
	io.WriteString(spec.Log, f.Output)
	if f.Err != nil {
		return "", f.Err
	}
	out, err := os.MkdirTemp(f.Parent, "release")
	if err != nil {
		return "", err
	}
	for _, l := range leaves {
		d := filepath.Join(out, l)
		os.MkdirAll(d, 0o755)
		if f.Make != nil {
			f.Make(f.t, d)
		} else {
			os.WriteFile(filepath.Join(d, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
			os.WriteFile(filepath.Join(d, "app.bin"), []byte("bin "+l), 0o644)
		}
	}
	return out, nil
}

func filesOf(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if rel, _ := filepath.Rel(dir, p); !d.IsDir() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

// releaseRun starts a run of leaves repos/api and repos/web with the stand-in releaser.
func releaseRun(t *testing.T) (*Run, *fakeReleaser) {
	t.Helper()
	realm := toySandbox(t)
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return &sandboxfake.Sandbox{}, nil }}
	r, err := Start(ctx, Options{Project: treetest.Project(t, "repos/api", "repos/web"), Config: sandboxConfig(t, "{ agent: scripted, model: none, script: "+filepath.Join(realm, "script.yaml")+" }", ""), Graph: realm, Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	for _, leaf := range []string{"repos/api", "repos/web"} {
		if err := r.AddLeaf(ctx, leaf); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeReleaser{t: t, Parent: t.TempDir()}
	r.Releaser = f
	return r, f
}

// commitInLeaf commits a file to the leaf and records the commit in the project umbrella.
func commitInLeaf(t *testing.T, r *Run, leaf, name, body string, mode os.FileMode) {
	t.Helper()
	l, err := r.Leaf(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.RepoDir(leaf), name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(r.RepoDir(leaf), name), mode); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", "add " + name}} {
		if _, err := l.Git(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	p := git.Repo{GitDir: r.Setup.Project.GitDir, WorkTree: r.Setup.Project.Dir}
	for _, args := range [][]string{{"add", leaf}, {"commit", "-q", "-m", "bump " + leaf}} {
		if _, err := p.Run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportAppAddsTheRelease(t *testing.T) {
	r, f := releaseRun(t)
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	commitInLeaf(t, r, "repos/web", "release.sh", "#!/bin/sh\n", 0o644) // not executable: no release
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("ExportApp() = %v", err)
	}
	files := filesOf(t, dst)
	for _, want := range []string{"umbrella.yaml", "repos/api/README.md", "repos/api/release.sh", ".ygg/release/repos/api/run.sh", ".ygg/release/repos/api/app.bin"} {
		if !slices.Contains(files, want) {
			t.Errorf("the export lacks %s: %v", want, files)
		}
	}
	for _, f := range files {
		if strings.HasPrefix(f, ".ygg/release/repos/web") {
			t.Errorf("a leaf with no executable release.sh has a release: %s", f)
		}
	}
	if len(f.Calls) != 1 || !slices.Equal(f.Calls[0], []string{"repos/api"}) {
		t.Errorf("releaser calls = %v", f.Calls)
	}
	if slices.ContainsFunc(f.Seen, func(s string) bool { return strings.HasPrefix(s, ".ygg/release") }) {
		t.Errorf("the releaser saw a release in its snapshot: %v", f.Seen)
	}
	if info, err := os.Stat(filepath.Join(dst, ".ygg/release/repos/api/run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh in the export = %v, %v; want it executable", info, err)
	}
	if st, _ := r.Leaf("repos/api"); st != nil {
		if out, _ := st.Git(ctx, "status", "--porcelain"); strings.Contains(out, ".ygg") {
			t.Errorf("the release is in the leaf: %s", out)
		}
	}
}

func TestReleaseIsReused(t *testing.T) {
	r, f := releaseRun(t)
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	dst := filepath.Join(r.GradeDir(), "app")
	for i := 0; i < 2; i++ {
		if err := r.ExportApp(ctx, dst); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(filesOf(t, dst), ".ygg/release/repos/api/app.bin") {
			t.Fatalf("export %d lacks the release", i)
		}
	}
	if len(f.Calls) != 1 {
		t.Errorf("two exports of the same commits called the releaser %d times", len(f.Calls))
	}
	commitInLeaf(t, r, "repos/api", "more.txt", "more\n", 0o644)
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Errorf("a new commit of a leaf called the releaser %d times in all, want 2", len(f.Calls))
	}
}

// A release kept from another releaser (another acceptance realm, version or image) is built again.
func TestReleaseIsKeptByItsReleaser(t *testing.T) {
	r, f := releaseRun(t)
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	dst := filepath.Join(r.GradeDir(), "app")
	for _, id := range []string{"midgard-react 0.1.0", "midgard-react 0.1.0", "midgard-react 0.2.0"} {
		f.ID = id
		if err := r.ExportApp(ctx, dst); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Calls) != 2 {
		t.Errorf("the releaser was called %d times; want once for each releaser", len(f.Calls))
	}
}

// release.log holds what the build printed, and the failure after it; past its bound it is cut.
func TestReleaseLogHoldsTheBuildsOutput(t *testing.T) {
	r, f := releaseRun(t)
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	f.Output, f.Err = "npm ci: added 12 packages\n", errors.New("vite: exit 1")
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err == nil {
		t.Fatal("ExportApp() = nil for a release that fails")
	}
	b, _ := os.ReadFile(filepath.Join(r.GradeDir(), "release.log"))
	if !strings.Contains(string(b), "npm ci: added 12 packages") || !strings.Contains(string(b), "vite: exit 1") {
		t.Errorf("release.log = %q", b)
	}
	f.Output, f.Err = strings.Repeat("x", maxReleaseLog+10), nil
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(r.GradeDir(), "release.log"))
	if len(b) > maxReleaseLog+100 || !strings.Contains(string(b), "release.log is cut here") || strings.Contains(string(b), "vite") {
		t.Errorf("release.log of a build that printed past the bound: %d bytes, ends %q", len(b), b[max(0, len(b)-80):])
	}
}

func TestReleaseRefused(t *testing.T) {
	cases := map[string]func(f *fakeReleaser){
		"a symlink": func(f *fakeReleaser) {
			f.Make = func(t *testing.T, d string) {
				if err := os.Symlink("/etc/passwd", filepath.Join(d, "link")); err != nil {
					t.Fatal(err)
				}
			}
		},
		"a fifo": func(f *fakeReleaser) {
			f.Make = func(t *testing.T, d string) {
				if err := syscallMkfifo(filepath.Join(d, "fifo")); err != nil {
					t.Skipf("no fifo here: %v", err)
				}
			}
		},
		"a release past the bound in all": func(f *fakeReleaser) {
			f.Make = func(t *testing.T, d string) {
				for _, n := range []string{"a", "b", "c", "d", "e"} {
					sparse(t, filepath.Join(d, n), maxReleaseFile)
				}
			}
		},
		"a releaser that fails": func(f *fakeReleaser) { f.Err = errors.New("go build: exit 2") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := releaseRun(t)
			setup(f)
			commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
			dst := filepath.Join(r.GradeDir(), "app")
			err := r.ExportApp(ctx, dst)
			var re *ReleaseError
			if !errors.As(err, &re) || !strings.Contains(err.Error(), "repos/api") {
				t.Fatalf("ExportApp() = %v, want a *ReleaseError that names repos/api", err)
			}
			if files := filesOf(t, dst); len(files) != 0 {
				t.Errorf("a partial export is left: %v", files)
			}
			if b, err := os.ReadFile(filepath.Join(r.GradeDir(), "release.log")); err != nil || len(b) == 0 {
				t.Errorf("release.log = %q, %v", b, err)
			} else if info, _ := os.Stat(filepath.Join(r.GradeDir(), "release.log")); info.Mode().Perm() != 0o600 {
				t.Errorf("release.log mode %v", info.Mode().Perm())
			}
			if _, err := os.Stat(filepath.Join(r.releaseDir())); err == nil {
				if es, _ := os.ReadDir(r.releaseDir()); len(es) != 0 {
					t.Errorf("a refused release is kept: %v", es)
				}
			}
		})
	}
}

func TestReleaseBound(t *testing.T) {
	if maxReleaseFileBytes != 128<<20 || maxReleaseTotalBytes != 512<<20 {
		t.Fatalf("the bounds are %d and %d, want 128 MiB and 512 MiB", maxReleaseFileBytes, maxReleaseTotalBytes)
	}
	r, f := releaseRun(t)
	f.Make = func(t *testing.T, d string) {
		b := make([]byte, 40<<20)
		b[0] = 'x'
		if err := os.WriteFile(filepath.Join(d, "app.bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("a 40 MiB release file: ExportApp() = %v", err)
	}
	if info, err := os.Stat(filepath.Join(dst, ".ygg/release/repos/api/app.bin")); err != nil || info.Size() != 40<<20 {
		t.Errorf("app.bin = %v, %v", info, err)
	}
	// One byte past the per-file bound is refused.
	r, f = releaseRun(t)
	f.Make = func(t *testing.T, d string) { sparse(t, filepath.Join(d, "big"), maxReleaseFile+1) }
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	var re *ReleaseError
	if err := r.ExportApp(ctx, dst); !errors.As(err, &re) || !strings.Contains(err.Error(), "big is") {
		t.Errorf("a file of %d bytes: ExportApp() = %v, want a *ReleaseError that names it", maxReleaseFile+1, err)
	}
}

// A kept release is copied under the same bounds as a new one, counted across its leaves, and a copy that
// fails says so in release.log.
func TestAKeptReleaseIsBoundedInAll(t *testing.T) {
	r, f := releaseRun(t)
	f.Make = func(t *testing.T, d string) {
		if err := os.WriteFile(filepath.Join(d, "app.bin"), make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	commitInLeaf(t, r, "repos/web", "release.sh", "#!/bin/sh\n", 0o755)
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatal(err)
	}
	defer func(old int64) { maxReleaseTotal = old }(maxReleaseTotal)
	maxReleaseTotal = 3 << 19 // one leaf's release fits, both do not
	err := r.ExportApp(ctx, dst)
	var re *ReleaseError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "more than") || len(f.Calls) != 1 {
		t.Fatalf("ExportApp() of a kept release past the bound in all = %v (releaser calls %d)", err, len(f.Calls))
	}
	if b, _ := os.ReadFile(filepath.Join(r.GradeDir(), "release.log")); !strings.Contains(string(b), "more than") {
		t.Errorf("release.log lacks the copy's failure:\n%s", b)
	}
}

func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := fh.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// linkReleaser makes a release whose path to the leaf folder passes through a symlink: <out>/repos is a
// relative link to a folder outside the release that holds api/host-secret.txt.
type linkReleaser struct{ Parent string }

func (l linkReleaser) Key() string { return "link" }

func (l linkReleaser) Build(context.Context, ReleaseSpec) (string, error) {
	out, outside := filepath.Join(l.Parent, "out"), filepath.Join(l.Parent, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "api"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(outside, "api", "host-secret.txt"), []byte("of the host\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	return out, os.Symlink(filepath.Join("..", "outside"), filepath.Join(out, "repos"))
}

// A link anywhere on the path from the release's folder down to a leaf's would take the copy outside the
// release: it is refused, and it is named, before anything is copied.
func TestALinkOnTheReleasesPathIsRefused(t *testing.T) {
	r, _ := releaseRun(t)
	r.Releaser = linkReleaser{Parent: t.TempDir()}
	commitInLeaf(t, r, "repos/api", "release.sh", "#!/bin/sh\n", 0o755)
	dst := filepath.Join(r.GradeDir(), "app")
	err := r.ExportApp(ctx, dst)
	var re *ReleaseError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "repos is a symlink") {
		t.Fatalf("ExportApp() = %v, want a *ReleaseError that names the link", err)
	}
	if files := filesOf(t, dst); len(files) != 0 {
		t.Errorf("the export holds %v", files)
	}
	if es, _ := os.ReadDir(r.releaseDir()); len(es) != 0 {
		t.Errorf("a release is kept: %v", es)
	}
}
