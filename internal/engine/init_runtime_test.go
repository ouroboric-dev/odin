package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// runtimeFiles is a runtime realm: a service for each need it provides, and provides. It names no leaf.
func runtimeFiles(name, version string, provides ...string) realmFiles {
	images := map[string]string{"postgres": toyPG, "s3": toyS3}
	ports := map[string]int{"postgres": 5432, "s3": 8333}
	pack := "services:\n"
	for _, n := range provides {
		pack += fmt.Sprintf("  %s:\n    image: %s\n    ports: [%d]\n", n, images[n], ports[n])
	}
	pack += "provides:\n"
	for _, n := range provides {
		pack += "  " + n + ": " + n + "\n"
	}
	return realmFiles{name: name, version: version, pack: pack}
}

// withNeeds is the realm rf needing needs.
func withNeeds(rf realmFiles, needs ...string) realmFiles {
	rf.pack += "needs: [" + strings.Join(needs, ", ") + "]\n"
	return rf
}

// runtimeLine is the runtime pin of a leaves file.
func runtimeLine(name, version string) string {
	return "runtime:\n  realm: " + name + "\n  realm_version: " + version + "\n"
}

// writeLeaves writes the project's leaves file.
func writeLeaves(t *testing.T, r *run.Run, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git1(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInitRecordsTheRuntimePin(t *testing.T) {
	ctx := context.Background()
	realms := append(galleryRealms(), runtimeFiles("niflheim-local", "0.1.0", "postgres", "s3"))
	r := initRun(t, runtimeLine("niflheim-local", "0.1.0")+galleryLeaves, realms...)
	if out, err := Advance(ctx, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	u, err := schema.LoadUmbrella(r.Project)
	if err != nil || u.Runtime != "niflheim-local" || u.RuntimeVersion != "0.1.0" {
		t.Fatalf("umbrella.yaml = %+v, %v; want runtime niflheim-local 0.1.0", u, err)
	}
	if r.Setup.Runtime == nil || r.Setup.Runtime.Name != "niflheim-local" || r.State.Realms["niflheim-local"].Version != "0.1.0" {
		t.Errorf("the run's runtime = %+v, run.json realms %+v", r.Setup.Runtime, r.State.Realms)
	}
	// The pin is in the commit that records the first leaf, and in no commit of its own.
	if got := git1(t, r.Project, "log", "--format=%s", "-S", "runtime: niflheim-local", "--", "umbrella.yaml"); !strings.HasPrefix(got, "Create leaf repos/api ") {
		t.Errorf("the runtime pin came in %q; want the commit that records repos/api", got)
	}
	if st := git1(t, r.Project, "status", "--porcelain", "--", "umbrella.yaml"); st != "" {
		t.Errorf("umbrella.yaml is left uncommitted: %q", st)
	}
	// Run again, init adds no commit.
	head := git1(t, r.Project, "rev-parse", "HEAD")
	if err := runInit(ctx, r, schema.Node{ID: "leaves", Kind: schema.KindInit, From: "docs/leaves.yaml"}); err != nil {
		t.Fatalf("init run again = %v", err)
	}
	if again := git1(t, r.Project, "rev-parse", "HEAD"); again != head {
		t.Errorf("init run again moved the project from %s to %s", head, again)
	}

	// A project whose leaves are all there gets the pin in a commit of its own.
	have := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n", realms: realms, leaves: map[string]string{"repos/api": "helheim-go"}})
	writeLeaves(t, have, runtimeLine("niflheim-local", "0.1.0")+"leaves:\n  repos/api:\n    realm: helheim-go\n    realm_version: 0.2.0\n    description: The API.\n")
	if out, err := Advance(ctx, have); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() with the leaves there = %+v, %v", out, err)
	}
	if u, _ := schema.LoadUmbrella(have.Project); u.Runtime != "niflheim-local" || u.RuntimeVersion != "0.1.0" {
		t.Errorf("umbrella.yaml = %+v; want the runtime pinned", u)
	}
	if got := git1(t, have.Project, "log", "-1", "--format=%s", "--", "umbrella.yaml"); got != "ygg init: pin the runtime realm niflheim-local 0.1.0" {
		t.Errorf("the pin's commit says %q", got)
	}

	// Once recorded, a leaves file names the same pin or none; another is refused, with both pins named.
	realms = append(realms, runtimeFiles("niflheim-cloud", "0.1.0", "postgres", "s3"))
	for leaves, want := range map[string]string{
		runtimeLine("niflheim-local", "0.1.0") + galleryLeaves: "",
		galleryLeaves: "",
		runtimeLine("niflheim-cloud", "0.1.0") + galleryLeaves: "the leaves file pins the runtime realm niflheim-cloud 0.1.0, and the project records niflheim-local 0.1.0",
	} {
		pinned := startRealms(t, realmsSetup{nodes: initNode, script: "steps: {}\n", realms: realms, runtime: "niflheim-local"})
		writeLeaves(t, pinned, leaves)
		out, err := Advance(ctx, pinned)
		u, _ := schema.LoadUmbrella(pinned.Project)
		if want == "" {
			if err != nil || out.Status != run.StatusDone || u.Runtime != "niflheim-local" || len(u.Children) != 2 {
				t.Errorf("%q: Advance() = %+v, %v; umbrella %+v", firstLine(leaves), out, err, u)
			}
			continue
		}
		if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), want) {
			t.Errorf("another runtime: Advance() = %+v, %v; want %q", out, err, want)
		}
		if len(u.Children) != 0 || u.Runtime != "niflheim-local" {
			t.Errorf("a refused runtime pin changed the project: %+v", u)
		}
	}
}

// unmetIn checks that the run failed for the need of the realm, in the words the operator acts on.
func unmetIn(t *testing.T, what string, out Outcome, err error, need, realmName string) {
	t.Helper()
	if err != nil || out.Status != run.StatusFailed || out.Err == nil {
		t.Fatalf("%s: Advance() = %+v, %v; want a failed run", what, out, err)
	}
	for _, want := range []string{"the realm " + realmName + " needs " + need, "runtime in docs/leaves.yaml"} {
		if !strings.Contains(out.Err.Error(), want) {
			t.Errorf("%s: %q does not say %q", what, out.Err, want)
		}
	}
}

// needyRealms are the gallery's realms with an API realm that needs postgres and s3, and two runtime
// realms: one that provides both and one that provides postgres alone.
func needyRealms() []realmFiles {
	return []realmFiles{recipeRealm("midgard-react", "0.1.0", "web.txt", "Web."), withNeeds(recipeRealm("helheim-go", "0.2.0", "api.txt", "API."), "postgres", "s3"),
		runtimeFiles("niflheim-local", "0.1.0", "postgres", "s3"), runtimeFiles("niflheim-half", "0.1.0", "postgres")}
}

func TestInitStopsOnAnUnmetNeedBeforeItCreatesALeaf(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, runtime, need, want string
	}{
		{name: "no runtime realm", need: "postgres"},
		{name: "no runtime realm", need: "s3"},
		{name: "a runtime realm without s3", runtime: runtimeLine("niflheim-half", "0.1.0"), need: "s3"},
		{name: "a build realm as the runtime", runtime: runtimeLine("helheim-go", "0.2.0"), want: "build realm"},
	} {
		r := initRun(t, c.runtime+galleryLeaves, needyRealms()...)
		out, err := Advance(ctx, r)
		if c.want != "" {
			if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), c.want) {
				t.Errorf("%s: Advance() = %+v, %v; want %q", c.name, out, err, c.want)
			}
		} else {
			unmetIn(t, c.name, out, err, c.need, "helheim-go")
		}
		if c.runtime != "" && c.want == "" && strings.Contains(out.Err.Error(), "needs postgres") {
			t.Errorf("%s: %q names postgres, which the runtime realm provides", c.name, out.Err)
		}
		if u, _ := schema.LoadUmbrella(r.Project); len(u.Children) != 0 || u.Runtime != "" {
			t.Errorf("%s: init changed the project before refusing: %+v", c.name, u)
		}
		if _, took := r.State.Realms["helheim-go"]; took {
			t.Errorf("%s: the run took the realms of a leaves file it refused: %v", c.name, r.State.Realms)
		}
	}
}
