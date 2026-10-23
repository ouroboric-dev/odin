package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/snapshot"
)

// What a release may hold. A release is a built product (a binary, a bundle), so the bounds are wider than
// the snapshot's for source (32 MiB a file, 256 MiB in all). The real releases measure 15 MiB (the API
// binary) and under 1 MiB (the web bundle); a later commit adjusts the bounds if a size comes near.
const (
	maxReleaseFileBytes  int64 = 128 << 20
	maxReleaseTotalBytes int64 = 512 << 20
)

// The bounds as variables, so that a test can narrow them.
var (
	maxReleaseFile  = maxReleaseFileBytes
	maxReleaseTotal = maxReleaseTotalBytes
)

// releaseScript is the file a leaf holds, executable, when it has a release (spec: the release in the
// export). The leaf's realm decides what it builds.
const releaseScript = "release.sh"

// Releaser builds the releases of the leaves of a snapshot (spec §11: the release at export). It returns a
// folder that holds each leaf's release at the leaf's path (<dir>/repos/api/...). The folder is the
// releaser's: Odin copies it, under the bounds above, and never keeps it. Anything the folder holds outside
// the leaves' paths is ignored. Key names what builds the releases, so that a release kept from another
// releaser (another realm, version or image) is built again; the sandbox releaser's covers the acceptance
// realm, its version and hash, and its acceptance image.
type Releaser interface {
	Key() string
	Build(ctx context.Context, spec ReleaseSpec) (dir string, err error)
}

// ReleaseSpec is what a release is built from.
type ReleaseSpec struct {
	Snapshot string            // the exported project, which the releaser may read and must not change
	Leaves   []string          // the paths of the leaves that hold an executable release.sh, sorted
	Revs     map[string]string // the commit the export holds of every leaf of the project, by its path
	Log      io.Writer         // the build's output, for the operator: release.log, beside grade.log
}

// ReleaseError is a release that could not be made or that the export refuses. It names the leaves.
type ReleaseError struct {
	Leaves []string
	Err    error
}

func (e *ReleaseError) Error() string {
	return fmt.Sprintf("the release of %s could not be made: %v", strings.Join(e.Leaves, ", "), e.Err)
}
func (e *ReleaseError) Unwrap() error { return e.Err }

// releaseDir is where the releases of a run are kept, each under the key of the commits it was built from.
func (r *Run) releaseDir() string { return filepath.Join(r.Dir, "release") }

// addRelease puts the release of each leaf of sources that has one into the export at dst, under
// .ygg/release/<leaf path>/. A release is built once for a set of commits and a releaser (its Key), and
// kept in the run folder; an export of the same commits copies it again. Nothing is committed to the
// project or a leaf.
func (r *Run) addRelease(ctx context.Context, dst string, sources []snapshot.Source) error {
	if r.Releaser == nil {
		return nil
	}
	spec, commits, err := releaseSpecOf(ctx, sources)
	if err != nil {
		return err
	}
	spec.Snapshot = dst
	if len(spec.Leaves) == 0 {
		return nil
	}
	key := sha256.New()
	fmt.Fprintf(key, "releaser %q\n", r.Releaser.Key())
	for _, c := range commits {
		fmt.Fprintln(key, c)
	}
	kept := filepath.Join(r.releaseDir(), hex.EncodeToString(key.Sum(nil))[:32])
	if _, err := os.Stat(kept); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.buildRelease(ctx, spec, kept); err != nil {
			return &ReleaseError{spec.Leaves, err}
		}
	}
	for _, leaf := range spec.Leaves {
		if err := copyRelease(filepath.Join(kept, filepath.FromSlash(leaf)), filepath.Join(dst, ".ygg", "release", filepath.FromSlash(leaf)), nil); err != nil {
			return &ReleaseError{[]string{leaf}, err}
		}
	}
	return nil
}

// releaseSpecOf reads the commit the export holds of each of sources, as "<path> <commit>" lines in the
// order of sources, and the spec of their release: the commit of every leaf, and the leaves that ship an
// executable release.sh at it, sorted.
func releaseSpecOf(ctx context.Context, sources []snapshot.Source) (spec ReleaseSpec, commits []string, err error) {
	spec.Revs = map[string]string{}
	for _, s := range sources {
		rev := s.Rev
		if rev == "" {
			out, err := s.Repo.Run(ctx, "rev-parse", "HEAD")
			if err != nil {
				return spec, nil, err
			}
			rev = strings.TrimSpace(out)
		}
		commits = append(commits, s.Rel+" "+rev)
		if s.Rel == "." {
			continue
		}
		spec.Revs[filepath.ToSlash(s.Rel)] = rev
		out, err := s.Repo.Run(ctx, "ls-tree", rev, "--", releaseScript)
		if err != nil {
			return spec, nil, err
		}
		if strings.HasPrefix(out, "100755 ") {
			spec.Leaves = append(spec.Leaves, filepath.ToSlash(s.Rel))
		}
	}
	slices.Sort(spec.Leaves)
	return spec, commits, nil
}

// NoReleaseNote says why the export holds no release when a leaf ships an executable release.sh and the
// run has no releaser: "no release: the project has no acceptance realm; …". It is "" when there is a
// releaser or no leaf ships a release.sh. Grade puts it in the ledger and both commands print it.
func (r *Run) NoReleaseNote(ctx context.Context) (string, error) {
	if r.Releaser != nil {
		return "", nil
	}
	sources, err := r.snapshotSources(ctx, nil)
	if err != nil {
		return "", &SnapshotError{err}
	}
	spec, _, err := releaseSpecOf(ctx, sources)
	if err != nil || len(spec.Leaves) == 0 {
		return "", err
	}
	why := "the project has no acceptance realm"
	if r.Sandbox == nil {
		why = "the run has no sandbox"
	}
	return fmt.Sprintf("no release: %s, so the export holds no release of %s, which ship an executable release.sh", why, strings.Join(spec.Leaves, ", ")), nil
}

// maxReleaseLog is the most of a build's output release.log keeps.
const maxReleaseLog = 4 << 20

// buildRelease has the releaser build the leaves of spec, checks what it made and keeps a copy at kept.
// Odin makes the copy itself, so every file of it belongs to the host user, whoever owned what the
// releaser made (B-139). The build's output goes to release.log, beside grade.log, and a failure's message
// after it; a failure leaves nothing at kept.
func (r *Run) buildRelease(ctx context.Context, spec ReleaseSpec, kept string) (err error) {
	if err := os.MkdirAll(r.GradeDir(), 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(r.GradeDir(), "release.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			fmt.Fprintf(log, "\n%v\n", err)
		}
		if cerr := log.Close(); err == nil {
			err = cerr
		}
	}()
	spec.Log = &cappedWriter{w: log, left: maxReleaseLog}
	out, err := r.Releaser.Build(ctx, spec)
	if err != nil {
		return err
	}
	tmp := kept + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var total int64
	for _, leaf := range spec.Leaves {
		if err := copyRelease(filepath.Join(out, filepath.FromSlash(leaf)), filepath.Join(tmp, filepath.FromSlash(leaf)), &total); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("%s: %w", leaf, err)
		}
	}
	if err := os.Rename(tmp, kept); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// cappedWriter writes to w until left bytes are written, then notes the cut once and drops the rest. It
// never fails a write: the log of a build must not fail the build.
type cappedWriter struct {
	w    io.Writer
	left int64
	cut  bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	n := min(int64(len(p)), c.left)
	c.w.Write(p[:n])
	c.left -= n
	if n < int64(len(p)) && !c.cut {
		c.cut = true
		fmt.Fprintf(c.w, "\n[release.log is cut here, at %d bytes]\n", maxReleaseLog)
	}
	return len(p), nil
}

// defaultReleaseTimeout is how long the whole release may take, sandbox start included, when the config
// has no grader section (ygg bridge); with one, the release has the grader's timeout.
const defaultReleaseTimeout = 20 * time.Minute

// releaseNode names the release's snapshot sandbox, as a node names the snapshot sandbox of its acceptance
// checks: snapshot/release/<realm>, in the run folder's snapshot/release/<realm>.
const releaseNode = "release"

// sandboxReleaser builds the releases in the project's acceptance sandbox (spec §11): the sandbox of the
// acceptance realm's acceptance stack, over a snapshot of the exported commits, with no agent, no secret
// and no service (the founder's decision, 2026-10-23: no build needs Postgres or S3). It runs each leaf's release.sh in the leaf, with OUT set to an empty folder of the snapshot and
// nothing else, and stops the sandbox before it returns, whether the release was made or not, so that the
// grader never starts beside it. Midgard-react's acceptance stack holds Node and the Go toolchain (it builds
// the API leaf for the acceptance run), and the snapshot sandbox opens the registries of every leaf's
// realm, so one sandbox builds the releases of both realms' leaves.
type sandboxReleaser struct {
	r       *Run
	realm   *realm.Realm
	timeout time.Duration
}

// SandboxReleaser is the releaser of an export (ygg grade, ygg bridge): the project's acceptance sandbox's.
// It is nil when the run has no sandbox or no realm of the project's leaves has an acceptance sandbox: the
// export then holds no release, as before releases.
func (r *Run) SandboxReleaser() (Releaser, error) {
	if r.Sandbox == nil {
		return nil, nil
	}
	rl, err := r.Setup.releaseRealm()
	if err != nil || rl == nil {
		return nil, err
	}
	timeout := defaultReleaseTimeout
	if g := r.Setup.Config.Grader; g != nil && g.Timeout > 0 {
		timeout = time.Duration(g.Timeout)
	}
	return &sandboxReleaser{r: r, realm: rl, timeout: timeout}, nil
}

// releaseRealm is the realm whose acceptance sandbox builds the releases: of the realms of the project's
// leaves that define an acceptance check a node of the graph runs, the first by name that names an
// acceptance stack (midgard-react, for the client gallery). It is nil when there is none.
func (s *Setup) releaseRealm() (*realm.Realm, error) {
	var found []*realm.Realm
	for _, n := range s.Graph.All() {
		for _, c := range n.Acceptance {
			realms, err := s.AcceptanceRealms(c)
			if err != nil {
				return nil, err
			}
			for _, rl := range realms {
				if rl.Acceptance != "" {
					found = append(found, rl)
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	return slices.MinFunc(found, func(a, b *realm.Realm) int { return strings.Compare(a.Name, b.Name) }), nil
}

// Key names the realm, its version and hash, and the acceptance image the releases are built on.
func (s *sandboxReleaser) Key() string {
	return fmt.Sprintf("acceptance sandbox of %s %s (%s) on %s", s.realm.Name, s.realm.Version, s.realm.Hash, s.realm.Acceptance)
}

// toolchainScript prints, for release.log, the version of each toolchain a release.sh may build with, when
// the sandbox has it. It always exits 0: a missing toolchain is release.sh's failure to report.
const toolchainScript = `if command -v go >/dev/null 2>&1; then go version; fi
if command -v node >/dev/null 2>&1; then echo "node $(node --version)"; fi
true`

func (s *sandboxReleaser) Build(ctx context.Context, spec ReleaseSpec) (dir string, err error) {
	r := s.r
	snap := filepath.Join(r.Dir, "snapshot", releaseNode, s.realm.Name)
	bctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	defer func() {
		// Down even when the release timed out or was stopped. A sandbox that does not stop stays recorded,
		// so the grade refuses to start the grader beside it, and doctor can stop it. Then the snapshot is
		// the host user's again, to copy from and to empty for the next release (B-139).
		if derr := errors.Join(r.DownSandboxes(context.WithoutCancel(ctx)), giveBack(snap)); derr != nil {
			dir, err = "", errors.Join(err, derr)
		}
	}()
	ended := func(err error) error {
		if ctx.Err() == nil && bctx.Err() != nil {
			return fmt.Errorf("the release did not finish within %s", s.timeout)
		}
		return err
	}
	// No service: a build needs none, and a sandbox with services did not stop on the real engine (its
	// Down failed on a running postgres container), which would refuse every grade with a release.
	runner, err := r.snapshotRunner(bctx, releaseNode, s.realm, spec.Revs, false)
	if err != nil {
		return "", ended(err)
	}
	env := r.State.Sandboxes["snapshot/"+releaseNode+"/"+s.realm.Name]
	out := filepath.Join(snap, ".ygg", "release")
	if err := os.RemoveAll(out); err != nil {
		return "", err
	}
	for _, leaf := range spec.Leaves {
		if err := os.MkdirAll(filepath.Join(out, filepath.FromSlash(leaf)), 0o755); err != nil {
			return "", err
		}
		fmt.Fprintf(spec.Log, "== %s: release.sh\n", leaf)
		if _, err := runner.Run(bctx, gungnir.Cmd{Argv: []string{"sh", "-c", toolchainScript}, Dir: filepath.FromSlash(leaf),
			Stdout: spec.Log, Stderr: spec.Log}); err != nil {
			return "", fmt.Errorf("%s: %w", leaf, ended(err))
		}
		res, err := runner.Run(bctx, gungnir.Cmd{Argv: []string{"./" + releaseScript}, Dir: filepath.FromSlash(leaf),
			Env:    map[string]string{"OUT": path.Join(env.Workdir, ".ygg", "release", leaf)},
			Stdout: spec.Log, Stderr: spec.Log})
		switch {
		case err != nil:
			return "", fmt.Errorf("%s: %w", leaf, ended(err))
		case res.ExitCode != 0:
			return "", fmt.Errorf("%s: release.sh exited %d (its output is in release.log)", leaf, res.ExitCode)
		}
	}
	return out, nil
}

// giveBack makes every folder under dir one the host user can read, enter and empty (before WalkDir reads
// it), and every file one it can read: a build may leave a folder read-only, which would stop the next
// snapshot from emptying dir. It follows no link. An entry the host user cannot change, such as one a
// sandbox's subuid owns, is an error that names it.
func giveBack(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // no snapshot was made
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		want := fs.FileMode(0o400)
		if d.IsDir() {
			want = 0o700
		} else if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&want != want {
			if err := os.Chmod(p, perm|want); err != nil {
				return fmt.Errorf("the release's snapshot holds %s, which the host user cannot take back: %w", p, err)
			}
		}
		return nil
	})
}

// copyRelease copies the folder from to to. Only regular files and folders are copied: a symlink, a device
// or any other entry is refused, and so is a file past the per-file bound, or a total (counted into total,
// when it is not nil) past the bound of all. A release is checked whole before a byte is copied.
func copyRelease(from, to string, total *int64) error {
	if total == nil {
		total = new(int64)
	}
	if err := checkRelease(from, total); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, info.Mode().Perm()&0o755|0o600)
	})
}

// checkRelease walks the folder and refuses what a release may not hold, adding the bytes it holds to total.
func checkRelease(dir string, total *int64) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the releaser made no release: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the release is not a folder")
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case !t.IsRegular():
			return fmt.Errorf("%s is a %s; a release holds only files and folders", rel, kindOf(t))
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxReleaseFile {
			return fmt.Errorf("%s is %d bytes; a release file may hold %d", rel, info.Size(), maxReleaseFile)
		}
		if *total += info.Size(); *total > maxReleaseTotal {
			return fmt.Errorf("the release holds more than %d bytes", maxReleaseTotal)
		}
		return nil
	})
}

func kindOf(t fs.FileMode) string {
	switch {
	case t&fs.ModeSymlink != 0:
		return "symlink"
	case t&fs.ModeDevice != 0:
		return "device file"
	default:
		return "special file"
	}
}

func copyFile(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
