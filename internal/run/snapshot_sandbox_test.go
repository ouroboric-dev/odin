package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
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
	root := treetest.Node{Owner: "ouroboric-dev", Visibility: "private", Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}, Children: map[string]treetest.Node{
		"repos/app": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "midgard-react", Version: "0.1.0"}},
		"repos/api": {Owner: "ouroboric-dev", Visibility: "private", Leaf: true, Pin: treetest.Pin{Realm: "helheim-go", Version: "0.2.0"}},
	}}
	fake := &sandboxfake.Sandbox{}
	h := Hooks{NewSandbox: func(*SandboxSetup, string) (gungnir.Sandbox, error) { return fake, nil }}
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	r, err := Start(ctx, Options{Project: treetest.Build(t, root), Config: sandboxConfig(t, script, "realms:\n  dir: "+store+"\n"), Realm: "midgard-react", Hooks: h})
	if err == nil {
		t.Cleanup(func() { r.Close() })
	}
	return r, fake, err
}

func TestTheSnapshotSandboxGetsTheServicesAndRegistriesOfEveryLeafsRealm(t *testing.T) {
	r, fake, err := snapshotRun(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", nil); err != nil {
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
	if up.Name != "snapshot/move" || !up.Scratch || up.Stack != nodeStack || strings.Join(names, ",") != "postgres,seaweedfs" ||
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
	for i := 0; i < 2; i++ {
		if _, err := r.SnapshotRunner(ctx, "move", nil); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			b, err := os.ReadFile(filepath.Join(r.Dir, "snapshot", "move", "repos", "app", "README.md"))
			if err != nil || len(b) == 0 {
				t.Fatalf("the snapshot lacks repos/app/README.md: %v", err)
			}
			if _, err := os.Stat(filepath.Join(r.Dir, "snapshot", "move", "repos", "api", "README.md")); err != nil {
				t.Errorf("the snapshot holds only the leaves the run works in: %v", err)
			}
			// A commit made in the leaf between the two snapshots is in the second one.
			dir := r.RepoDir("repos/app")
			os.WriteFile(filepath.Join(dir, "later.txt"), []byte("later\n"), 0o644)
			for _, args := range [][]string{{"add", "later.txt"}, {"commit", "-q", "-m", "later"}} {
				l, _ := r.Leaf("repos/app")
				if _, err := l.Git(ctx, args...); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "snapshot", "move", "repos", "app", "later.txt")); err != nil {
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
	if _, err := r.SnapshotRunner(ctx, "move", nil); err == nil || !strings.Contains(err.Error(), "two different services named postgres") || !strings.Contains(err.Error(), "the snapshot") {
		t.Errorf("SnapshotRunner() = %v", err)
	}
}

func TestARealmWithNoAcceptanceStackHasNoSnapshotSandbox(t *testing.T) {
	r, fake, err := snapshotRun(t, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRunner(ctx, "move", nil); err == nil || !strings.Contains(err.Error(), "names no acceptance stack") {
		t.Errorf("SnapshotRunner() = %v", err)
	}
	if len(fake.Ups) != 0 {
		t.Error("a sandbox came up without a stack for it")
	}
}
