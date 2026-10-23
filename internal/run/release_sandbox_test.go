package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// releaseScriptBody is a leaf's release.sh for these tests: it says where it runs, writes to both of its
// outputs, records its environment in the release, and leaves a read-only folder in OUT and in the leaf,
// as a build can (B-139: a second export must still remove what the first made).
const releaseScriptBody = `#!/bin/sh
set -eu
echo "building $(basename "$PWD")"
echo "a warning of $(basename "$PWD")" >&2
mkdir -p "$OUT"
env | sort > "$OUT/env.txt"
printf '#!/bin/sh\n' > "$OUT/run.sh"
chmod 755 "$OUT/run.sh"
mkdir -p "$OUT/ro" ro-built
echo built > "$OUT/ro/file"
chmod 555 "$OUT/ro" ro-built
`

// releaseSandboxRun starts a run of a web leaf (midgard-react) and an api leaf (helheim-go) whose graph runs
// the acceptance check accept at its pointer move. With acceptance, midgard-react defines accept and names an
// acceptance stack, as the real realm does; without it, no realm of the project defines an acceptance check.
// Both leaves commit an executable release.sh.
func releaseSandboxRun(t *testing.T, acceptance bool) (*Run, *sandboxfake.Sandbox) {
	t.Helper()
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", "")
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org, sum.golang.org", apiServices)
	if acceptance {
		withAcceptance(t, store, "midgard-react")
		replaceIn(t, filepath.Join(store, "midgard-react", "pack.yaml"), "checks:\n", "checks:\n  accept: [\"true\"]\n")
		replaceIn(t, filepath.Join(store, "midgard-react", "graph.yaml"), "kind: pointer, repo: repos/app, checks: [unit] }", "kind: pointer, repo: repos/app, checks: [unit], acceptance: [accept] }")
	}
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: sandboxConfig(t, script, "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n"), Graph: "midgard-react", Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	for _, leaf := range []string{"repos/api", "repos/app"} {
		commitInLeaf(t, r, leaf, "release.sh", releaseScriptBody, 0o755)
	}
	return r, fake
}

func replaceIn(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s lacks %q", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// useSandboxReleaser gives the run the releaser grade and bridge give it.
func useSandboxReleaser(t *testing.T, r *Run) *sandboxReleaser {
	t.Helper()
	rel, err := r.SandboxReleaser()
	if err != nil {
		t.Fatalf("SandboxReleaser() = %v", err)
	}
	r.Releaser = rel
	s, _ := rel.(*sandboxReleaser)
	return s
}

func TestReleaseRunsInTheAcceptanceSandbox(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "fake-key-of-the-test") // a credential in Odin's own environment
	r, fake := releaseSandboxRun(t, true)
	fakeToolchain(t)
	if rel := useSandboxReleaser(t, r); rel == nil || rel.timeout != defaultReleaseTimeout {
		t.Fatalf("SandboxReleaser() = %+v; want the acceptance sandbox's, with the default timeout when there is no grader section", rel)
	}
	r.Setup.Config.Grader = &schema.Grader{Timeout: schema.Duration(7 * time.Minute)}
	if rel := useSandboxReleaser(t, r); rel == nil || rel.timeout != 7*time.Minute {
		t.Fatalf("SandboxReleaser() = %+v; want the grader's timeout", rel)
	}
	before := len(events(t, r))
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("ExportApp() = %v", err)
	}

	// One sandbox: the acceptance realm's, on its acceptance stack, over a snapshot of the exported commits.
	if len(fake.Ups) != 1 {
		t.Fatalf("%d sandboxes came up, want the one acceptance sandbox", len(fake.Ups))
	}
	up := fake.Ups[0]
	if up.Name != "snapshot/release/midgard-react" || !up.Scratch || up.Stack != nodeStack ||
		strings.Join(up.Egress, ",") != "proxy.golang.org,registry.npmjs.org,sum.golang.org" {
		t.Errorf("the release sandbox's spec = %+v", up)
	}
	// No build needs a service, though the api leaf's realm has postgres and seaweedfs: the release sandbox
	// starts none (the founder's decision, 2026-10-23), while the acceptance run's snapshot sandbox keeps them.
	if len(up.Services) != 0 {
		t.Errorf("the release sandbox starts services: %+v", up.Services)
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Ups); n != 2 || len(fake.Ups[1].Services) != 2 {
		t.Errorf("the acceptance run's snapshot sandbox: %d ups, %+v", n, fake.Ups[n-1].Services)
	}
	if err := r.DownSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	fake.Ups = fake.Ups[:1]
	// In each leaf, the toolchain's versions, then release.sh, with OUT and nothing else set, and nothing
	// else ran: no agent session.
	if len(fake.Execs) != 4 {
		t.Fatalf("%d commands ran in the sandbox, want two in each of the two leaves: %+v", len(fake.Execs), fake.Execs)
	}
	for i, leaf := range []string{"repos/api", "repos/app"} {
		probe, c := fake.Execs[2*i], fake.Execs[2*i+1]
		if probe.Argv[0] != "sh" || probe.Dir != leaf || len(probe.Env) != 0 {
			t.Errorf("command %d = %+v; want the toolchain's versions, in %s, with no variable", 2*i, probe, leaf)
		}
		want := filepath.Join(up.Leaf, ".ygg", "release", leaf)
		if !slices.Equal(c.Argv, []string{"./release.sh"}) || c.Dir != leaf || len(c.Env) != 1 || c.Env["OUT"] != want {
			t.Errorf("command %d = %+v; want ./release.sh in %s with OUT=%s alone", 2*i+1, c, leaf, want)
		}
	}
	for _, e := range events(t, r)[before:] {
		if e != string(gungnir.EventSandboxUp) && e != string(gungnir.EventSandboxDown) {
			t.Errorf("the release logged %s; it starts and stops a sandbox and nothing else", e)
		}
	}
	// The release is in the export, copied out of the sandbox's snapshot.
	files := filesOf(t, dst)
	for _, want := range []string{".ygg/release/repos/api/run.sh", ".ygg/release/repos/app/run.sh", ".ygg/release/repos/api/ro/file", "repos/api/release.sh"} {
		if !slices.Contains(files, want) {
			t.Errorf("the export lacks %s: %v", want, files)
		}
	}
	for _, f := range files {
		if strings.Contains(f, "ro-built") {
			t.Errorf("the export holds what release.sh left in the leaf: %s", f)
		}
	}
	env, err := os.ReadFile(filepath.Join(dst, ".ygg/release/repos/api/env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "OPENROUTER_API_KEY") || strings.Contains(string(env), "fake-key-of-the-test") {
		t.Errorf("a credential reached release.sh:\n%s", env)
	}
	// The build's output is the operator's, in release.log.
	logPath := filepath.Join(r.GradeDir(), "release.log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== repos/api: release.sh\ngo version go1.99.0 fake\nnode v99.0.0\n", "building api", "a warning of api",
		"== repos/app: release.sh\ngo version go1.99.0 fake\nnode v99.0.0\n", "building app"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("release.log lacks %q:\n%s", want, b)
		}
	}
	if info, _ := os.Stat(logPath); info.Mode().Perm() != 0o600 {
		t.Errorf("release.log mode %v", info.Mode().Perm())
	}
	// Copied out as the host user: the export holds only the operator's files, and a second export of other
	// commits builds again over the first one's snapshot and removes the first one's export (B-139).
	assertOwnAndRemovable(t, dst)
	commitInLeaf(t, r, "repos/api", "more.txt", "more\n", 0o644)
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("the second ExportApp() = %v", err)
	}
	if len(fake.Ups) != 2 {
		t.Errorf("a new commit started %d release sandboxes in all, want 2", len(fake.Ups))
	}
	if err := os.RemoveAll(dst); err != nil {
		t.Errorf("the export cannot be removed: %v", err)
	}
}

// fakeToolchain puts a go and a node that print made-up versions first on the PATH the stand-in sandbox
// runs commands with.
func fakeToolchain(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for name, version := range map[string]string{"go": "go version go1.99.0 fake", "node": "v99.0.0"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// assertOwnAndRemovable fails unless every entry under dir belongs to this user and every folder can be
// written, so that removing dir removes it all.
func assertOwnAndRemovable(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		info, err := d.Info()
		if err != nil {
			t.Fatal(err)
		}
		if uid, ok := ownerOf(info); ok && uid != os.Getuid() {
			t.Errorf("%s belongs to uid %d, not the host user %d", p, uid, os.Getuid())
		}
		if d.IsDir() && info.Mode().Perm()&0o300 != 0o300 {
			t.Errorf("%s is %v: the host user cannot empty it", p, info.Mode().Perm())
		}
		return nil
	})
}

// downFails is the stand-in engine with a sandbox that does not stop.
type downFails struct{ *sandboxfake.Sandbox }

func (d downFails) Down(context.Context, gungnir.Env) error {
	return errors.New("podman: the container does not stop")
}

func TestReleaseSandboxStopsBeforeTheGrader(t *testing.T) {
	cases := map[string]struct {
		script  string        // release.sh of repos/api; "" keeps the one that works
		timeout time.Duration // 0 keeps the grader's
		down    bool          // the sandbox does not stop
		want    string        // in ExportApp's error; "" is no error
	}{
		"a release that is made":    {},
		"a release.sh that fails":   {script: "#!/bin/sh\necho broken build >&2\nexit 3\n", want: "exited 3"},
		"a release that times out":  {script: "#!/bin/sh\nexec sleep 30\n", timeout: 200 * time.Millisecond, want: "did not finish within 200ms"},
		"a sandbox that won't stop": {down: true, want: "does not stop"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, fake := releaseSandboxRun(t, true)
			if tc.down {
				r.Sandbox = downFails{fake}
			}
			if tc.script != "" {
				commitInLeaf(t, r, "repos/api", "release.sh", tc.script, 0o755)
			}
			rel := useSandboxReleaser(t, r)
			if tc.timeout != 0 {
				rel.timeout = tc.timeout
			}
			dst := filepath.Join(r.GradeDir(), "app")
			err := r.ExportApp(ctx, dst)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ExportApp() = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ExportApp() = %v, want an error that says %q", err, tc.want)
			}
			if len(fake.Ups) != 1 {
				t.Fatalf("%d sandboxes came up", len(fake.Ups))
			}
			if tc.down {
				// The sandbox is still recorded, so the grade refuses to start the grader, and doctor can stop it.
				if r.NoSandboxesLeft() == nil {
					t.Error("a release sandbox that did not stop is not recorded; the grader would start beside it")
				}
				if files := filesOf(t, dst); len(files) != 0 {
					t.Errorf("an export is left for a grader: %v", files)
				}
				return
			}
			// What the grade checks right after the export, before it starts the grader's container.
			if err := r.NoSandboxesLeft(); err != nil {
				t.Errorf("NoSandboxesLeft() = %v after the export", err)
			}
			if len(fake.Downs) != 1 || fake.Downs[0].Name != "snapshot/release/midgard-react" {
				t.Errorf("sandboxes stopped: %+v", fake.Downs)
			}
			if tc.want != "" {
				b, _ := os.ReadFile(filepath.Join(r.GradeDir(), "release.log"))
				if !strings.Contains(string(b), tc.want) || (tc.timeout == 0 && !strings.Contains(string(b), "broken build")) {
					t.Errorf("release.log lacks the failure:\n%s", b)
				}
			}
		})
	}
}

func TestNoAcceptanceSandboxNoRelease(t *testing.T) {
	r, fake := releaseSandboxRun(t, false)
	if rel, err := r.SandboxReleaser(); err != nil || rel != nil {
		t.Fatalf("SandboxReleaser() = %v, %v; want none for a project with no acceptance realm", rel, err)
	}
	useSandboxReleaser(t, r)
	// The leaves ship a release.sh, so the missing release is said, not silent.
	if note, err := r.NoReleaseNote(ctx); err != nil || !strings.HasPrefix(note, "no release: the project has no acceptance realm") ||
		!strings.Contains(note, "repos/api, repos/app") {
		t.Errorf("NoReleaseNote() = %q, %v", note, err)
	}
	dst := filepath.Join(r.GradeDir(), "app")
	if err := r.ExportApp(ctx, dst); err != nil {
		t.Fatalf("ExportApp() = %v", err)
	}
	files := filesOf(t, dst)
	if !slices.Contains(files, "repos/api/release.sh") {
		t.Errorf("the export lacks the project: %v", files)
	}
	if slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, ".ygg/") }) {
		t.Errorf("the export holds a release: %v", files)
	}
	if len(fake.Ups) != 0 || len(fake.Execs) != 0 {
		t.Errorf("a sandbox came up (%d) or ran a command (%d)", len(fake.Ups), len(fake.Execs))
	}
	// And a run with no sandbox at all has no releaser either.
	r.Sandbox = nil
	if rel, err := r.SandboxReleaser(); err != nil || rel != nil {
		t.Errorf("with no sandbox: SandboxReleaser() = %v, %v", rel, err)
	}
}

func TestTheSandboxReleasersKeyNamesTheRealmAndItsImage(t *testing.T) {
	r, _ := releaseSandboxRun(t, true)
	rel := useSandboxReleaser(t, r)
	if note, err := r.NoReleaseNote(ctx); err != nil || note != "" {
		t.Errorf("with a releaser: NoReleaseNote() = %q, %v", note, err)
	}
	key := rel.Key()
	for _, want := range []string{"midgard-react", "0.1.0", nodeStack, rel.realm.Hash} {
		if !strings.Contains(key, want) {
			t.Errorf("Key() = %q lacks %q", key, want)
		}
	}
	other := *rel.realm
	other.Version = "0.1.1"
	if (&sandboxReleaser{realm: &other}).Key() == key {
		t.Error("another version of the acceptance realm has the same key")
	}
	other = *rel.realm
	other.Acceptance = "localhost/ygg-stack-midgard-react-acceptance:0123456789ab"
	if (&sandboxReleaser{realm: &other}).Key() == key {
		t.Error("another acceptance image has the same key")
	}
}
