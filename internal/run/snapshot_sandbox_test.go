package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// withAcceptance gives the realm named in store an acceptance stack.
func withAcceptance(t *testing.T, store, name string) {
	t.Helper()
	pack := filepath.Join(store, name, "pack.yaml")
	b, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, []byte(strings.Replace(string(b), "  egress:", "  acceptance: { stack: "+nodeStack+" }\n  egress:", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotRun starts a run of a web leaf and an api leaf that don't consume each other, so that only the
// snapshot, which holds both, sees both realms' services.
func snapshotRun(t *testing.T, webServices string, acceptance bool) (*Run, *sandboxfake.Sandbox, error) {
	t.Helper()
	store := t.TempDir()
	sandboxRealm(t, store, "midgard-react", "0.1.0", nodeStack, "registry.npmjs.org", webServices)
	sandboxRealm(t, store, "helheim-go", "0.2.0", nodeStack, "proxy.golang.org, sum.golang.org", apiServices)
	if acceptance {
		withAcceptance(t, store, "midgard-react")
	}
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", GraphPin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: sandboxConfig(t, script, "realms:\n  dir: "+store+"\ngraphs:\n  dir: "+store+"\n"), Graph: "midgard-react", Hooks: h})
	if err == nil {
		t.Cleanup(func() { r.Close() })
	}
	return r, fake, err
}

// webRealm is the realm of repos/app, whose acceptance stack the snapshot sandbox starts on.
func webRealm(t *testing.T, r *Run) *realm.Realm {
	t.Helper()
	rl, err := r.Setup.RealmOf("repos/app")
	if err != nil {
		t.Fatal(err)
	}
	return rl
}

func TestTheSnapshotSandboxGetsTheServicesAndRegistriesOfEveryLeafsRealm(t *testing.T) {
	r, fake, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err != nil {
		t.Fatalf("SnapshotRunner() = %v", err)
	}
	if len(fake.Ups) != 1 {
		t.Fatalf("%d sandboxes came up", len(fake.Ups))
	}
	up := fake.Ups[0]
	var names []string
	for _, v := range up.Services {
		names = append(names, v.Name)
	}
	if up.Name != "snapshot/move/midgard-react" || !up.Scratch || up.Stack != nodeStack || strings.Join(names, ",") != "postgres,seaweedfs" ||
		strings.Join(up.Egress, ",") != "proxy.golang.org,registry.npmjs.org,sum.golang.org" {
		t.Errorf("spec = %+v", up)
	}
	if err := up.Validate(); err != nil {
		t.Errorf("the spec is one the driver would refuse: %v", err)
	}
}

func TestTheSnapshotIsMadeAgainEveryTimeAndTheSandboxStartsOnce(t *testing.T) {
	r, fake, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var revs map[string]string
	for i := 0; i < 2; i++ {
		if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), revs); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			b, err := os.ReadFile(filepath.Join(r.Dir, "snapshot", "move", "midgard-react", "repos", "app", "README.md"))
			if err != nil || len(b) == 0 {
				t.Fatalf("the snapshot lacks repos/app/README.md: %v", err)
			}
			if _, err := os.Stat(filepath.Join(r.Dir, "snapshot", "move", "midgard-react", "repos", "api", "README.md")); err != nil {
				t.Errorf("the snapshot holds only the leaves the run works in: %v", err)
			}
			// A commit made in the leaf between the two snapshots is in the second one when the node moves the
			// leaf to it; a leaf the node does not move stays at the commit the umbrella records.
			dir := r.RepoDir("repos/app")
			os.WriteFile(filepath.Join(dir, "later.txt"), []byte("later\n"), 0o644)
			for _, args := range [][]string{{"add", "later.txt"}, {"commit", "-q", "-m", "later"}} {
				l, _ := r.Leaf("repos/app")
				if _, err := l.Git(ctx, args...); err != nil {
					t.Fatal(err)
				}
			}
			l, _ := r.Leaf("repos/app")
			head, err := l.Git(ctx, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			revs = map[string]string{"repos/app": strings.TrimSpace(head)}
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "snapshot", "move", "midgard-react", "repos", "app", "later.txt")); err != nil {
		t.Errorf("the second snapshot lacks the commit made after the first: %v", err)
	}
	if len(fake.Ups) != 1 {
		t.Errorf("%d sandboxes came up for one node", len(fake.Ups))
	}
}

func TestRealmsThatDisagreeOnAServiceCannotShareASnapshotSandbox(t *testing.T) {
	clash := "services:\n  postgres:\n    image: " + pgImage + "\n    env: { POSTGRES_DB: other }\n    ports: [5432]\n"
	r, _, err := snapshotRun(t, clash, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err == nil || !strings.Contains(err.Error(), "two different services named postgres") || !strings.Contains(err.Error(), "the snapshot") {
		t.Errorf("SnapshotRunner() = %v", err)
	}
}

// Prepare and init refuse such a realm before the run starts (TestARealmWithAnAcceptanceCheckAndNoAcceptanceStack...);
// this is the second line: a sandbox is never started for it.
func TestASnapshotSandboxIsNeverStartedForARealmWithNoAcceptanceStack(t *testing.T) {
	r, fake, err := snapshotRun(t, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err == nil || !strings.Contains(err.Error(), "names no acceptance stack") {
		t.Errorf("SnapshotRunner() = %v", err)
	}
	if len(fake.Ups) != 0 {
		t.Error("a sandbox came up without a stack for it")
	}
}

// B-048 M-1: the snapshot takes a leaf at the commit the umbrella records (the gitlink), not at the commit
// the leaf's working copy happens to be on. The leaf the node moves is the exception: revs names its commit.
func TestTheSnapshotTakesAnotherLeafAtTheCommitTheUmbrellaRecords(t *testing.T) {
	r, _, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.Leaf("repos/api")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.RepoDir("repos/api"), "later.txt"), []byte("past the recorded commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "later.txt"}, {"commit", "-q", "-m", "later"}} {
		if _, err := l.Git(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SnapshotRunner(ctx, "move", webRealm(t, r), nil); err != nil {
		t.Fatalf("SnapshotRunner() = %v", err)
	}
	snap := filepath.Join(r.Dir, "snapshot", "move", "midgard-react", "repos", "api")
	if _, err := os.Stat(filepath.Join(snap, "README.md")); err != nil {
		t.Errorf("the snapshot lacks the recorded commit's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snap, "later.txt")); err == nil {
		t.Error("the snapshot holds a commit the umbrella does not record; it took the leaf's HEAD")
	}
}

func TestTheSnapshotRefusesALeafThatIsNotCheckedOut(t *testing.T) {
	r, fake, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(r.Setup.Root, ".git", "modules", "repos", "api")); err != nil {
		t.Fatal(err)
	}
	_, err = r.SnapshotRunner(ctx, "move", webRealm(t, r), nil)
	var se *SnapshotError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "repos/api") || !strings.Contains(err.Error(), "not checked out") {
		t.Errorf("SnapshotRunner() = %v; want a *SnapshotError naming repos/api as not checked out", err)
	}
	if len(fake.Ups) != 0 {
		t.Error("a sandbox came up over a snapshot that lacks a leaf")
	}
}

func TestTheSnapshotRefusesALeafThatLacksTheRecordedCommit(t *testing.T) {
	r, _, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	// A leaf whose history was replaced: its objects no longer hold the commit the umbrella records.
	gitDir := filepath.Join(r.Setup.Root, ".git", "modules", "repos", "api")
	if err := os.RemoveAll(filepath.Join(gitDir, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = r.SnapshotRunner(ctx, "move", webRealm(t, r), nil)
	var se *SnapshotError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "repos/api") {
		t.Errorf("SnapshotRunner() = %v; want a *SnapshotError naming repos/api", err)
	}
}

// Init hands the run realms it planned, which are not the pointers the run loaded; the same realm reached
// twice is one realm, and its check runs once.
func TestTheSameRealmReachedThroughTwoPointersIsOneAcceptanceRealm(t *testing.T) {
	r, _, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	loaded := webRealm(t, r)
	planned := *loaded
	got, err := r.Setup.acceptanceRealms("unit", []*realm.Realm{&planned})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range got {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "helheim-go,midgard-react" {
		t.Errorf("acceptance realms = %v; want each realm once, by name", names)
	}
}
