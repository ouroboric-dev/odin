package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/deps"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// A bump in a project of three Go leaves whose realm (helheim-go, a toy of it) has the two rungs a bump runs:
// deps passes when go.mod is there, unit passes when ok.txt is there. The "go" the tool runs is a script
// first on PATH, so no test needs a network: it writes the version it is given into go.mod.

const fakeGo = `#!/bin/sh
v="${2#*@v}"
printf 'module m\n\ngo 1.26\n\nrequire golang.org/x/text v%s\n' "$v" > go.mod
`

// depsFakeGoExtra is more shell for the stand-in go to run after it wrote go.mod; a test sets it.
var depsFakeGoExtra string

func goMod(v string) string {
	return "module m\n\ngo 1.26\n\nrequire golang.org/x/text v" + v + "\n"
}

func depsProject(t *testing.T, stack string) (dir, config string) {
	t.Helper()
	return depsProjectWith(t, stack, "", false)
}

// depsProjectWith is depsProject with more lines in the leaves' realm pack (needs: [...]) and, when runtime is
// true, an umbrella that pins the runtime realm niflheim-local, which provides postgres and s3.
func depsProjectWith(t *testing.T, stack, packExtra string, runtime bool) (dir, config string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo+depsFakeGoExtra), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := t.TempDir()
	pack := "name: helheim-go\nversion: 0.1.0\nchecks:\n  deps: [test, -f, go.mod]\n  unit: [test, -f, ok.txt]\n"
	if stack != "" {
		pack += "sandbox:\n  stack: " + stack + "\n  egress: [proxy.golang.org]\n"
	}
	pack += packExtra
	realms := map[string]string{"helheim-go": pack}
	var rt treetest.Pin
	if runtime {
		realms["niflheim-local"] = "name: niflheim-local\nversion: 0.1.0\nservices:\n  postgres:\n    image: " + pgImage + "\n    ports: [5432]\n" +
			"  s3:\n    image: docker.io/chrislusf/seaweedfs:4.45@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5\n    ports: [8333]\n" +
			"provides:\n  postgres: postgres\n  s3: s3\n"
		rt = treetest.Pin{Realm: "niflheim-local", Version: "0.1.0"}
	}
	for name, body := range realms {
		if err := os.MkdirAll(filepath.Join(store, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, name, "pack.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pin := treetest.Pin{Realm: "helheim-go", Version: "0.1.0"}
	leaf := treetest.Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Leaf: true, Pin: pin}
	dir = treetest.Build(t, treetest.Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, GraphPin: pin, RuntimePin: rt,
		Children: map[string]treetest.Node{"repos/api": leaf, "repos/web": leaf, "repos/old": leaf}})
	for name, v := range map[string]string{"api": "0.41.0", "web": "0.41.0", "old": "0.43.0"} {
		ns, err := tree.Resolve(ctx, dir, "repos/"+name) // checks the leaf out
		if err != nil {
			t.Fatal(err)
		}
		for f, body := range map[string]string{"go.mod": goMod(v), "ok.txt": "ok\n"} {
			if err := os.WriteFile(filepath.Join(ns[len(ns)-1].Dir, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	commitFixtures(t, dir)
	_, base := project(t, "script.yaml", "")
	b, _ := os.ReadFile(base)
	extra := "realms:\n  dir: " + store + "\ngraphs:\n  dir: " + store + "\n"
	if stack != "" {
		extra += sandboxExtra
	}
	config = filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(config, append(b, []byte(extra)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, config
}

func mod(t *testing.T, dir, leaf string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "repos", leaf, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// bumpLedger returns the one bump's ledger, verified, or "" when the bump wrote none.
func bumpLedger(t *testing.T, dir string) string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, ".factory", "deps"))
	if len(entries) == 0 {
		return ""
	}
	p := filepath.Join(dir, ".factory", "deps", entries[0].Name(), "ledger.jsonl")
	if _, err := ledger.Verify(p, ledger.Anchor{}); err != nil {
		t.Fatalf("the ledger of the bump: %v", err)
	}
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestALeafThatPinsNoRealmIsRefusedWhateverTheUmbrellaPins(t *testing.T) {
	dir, config := depsProject(t, "")
	u, err := schema.LoadUmbrella(dir)
	if err != nil || u.Graph == "" {
		t.Fatalf("the umbrella = %+v, %v; the fixture pins a graph", u, err)
	}
	c := u.Children["repos/old"]
	c.Realm, c.RealmVersion = "", ""
	u.Children["repos/old"] = c
	if err := u.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "commit", "-q", "-am", "repos/old pins no realm"); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host",
		"--canary", "repos/old", "--rollout", "repos/web")
	if code != ExitRefused || !strings.Contains(errOut, "pins no realm") {
		t.Errorf("exit %d, want %d and %q:\n%s%s", code, ExitRefused, "pins no realm", out, errOut)
	}
}

func TestDepsBumpMovesTheCanaryThenTheRolloutProduct(t *testing.T) {
	dir, config := depsProject(t, "")
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host",
		"--canary", filepath.Join(dir, "repos/api"), "--rollout", "repos/web")
	if code != ExitDone || !strings.Contains(out, "repos/api (canary): 0.41.0 to 0.42.0: pass") || !strings.Contains(out, "repos/web (rollout): 0.41.0 to 0.42.0: pass") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if mod(t, dir, "api") != goMod("0.42.0") || mod(t, dir, "web") != goMod("0.42.0") || mod(t, dir, "old") != goMod("0.43.0") {
		t.Error("go.mod of api, web or old is wrong")
	}
	l := bumpLedger(t, dir)
	for _, want := range []string{`"event":"deps_checked"`, `"ygg.realm.name":"helheim-go"`, `"event":"deps_bump"`, `"ygg.deps.stage":"canary"`, `"ygg.deps.stage":"rollout"`, `"ygg.deps.package":"golang.org/x/text"`, `"ygg.verdict":"pass"`} {
		if !strings.Contains(l, want) {
			t.Errorf("ledger lacks %s:\n%s", want, l)
		}
	}
}

func TestDepsBumpPutsAFailedCanaryBackAndTouchesNoRolloutProduct(t *testing.T) {
	dir, config := depsProject(t, "")
	if err := os.Remove(filepath.Join(dir, "repos/api/ok.txt")); err != nil { // the unit rung of api now fails
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host", "--canary", "repos/api", "--rollout", "repos/web")
	if code != ExitFailed || !strings.Contains(out, "repos/api (canary): 0.41.0 to 0.42.0: fail (the check unit failed") || !strings.Contains(errOut, "repos/api failed") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if mod(t, dir, "api") != goMod("0.41.0") || mod(t, dir, "web") != goMod("0.41.0") {
		t.Error("a go.mod is not as it was")
	}
	l := bumpLedger(t, dir)
	if !strings.Contains(l, `"ygg.deps.stage":"canary"`) || !strings.Contains(l, `"ygg.verdict":"fail"`) || strings.Contains(l, `"rollout"`) {
		t.Errorf("ledger:\n%s", l)
	}
}

func TestDepsBumpRefusalsExitTwoAndLeaveNoTrace(t *testing.T) {
	dir, config := depsProject(t, "")
	common := []string{"--project", dir, "--config", config, "--host"}
	cases := map[string]struct {
		args []string
		want string
	}{
		"a bump down":           {[]string{"deps", "bump", "golang.org/x/text", "0.40.0", "--canary", "repos/api"}, "goes down"},
		"a rollout bump down":   {[]string{"deps", "bump", "golang.org/x/text", "0.42.0", "--canary", "repos/api", "--rollout", "repos/old"}, "goes down"},
		"no canary":             {[]string{"deps", "bump", "golang.org/x/text", "0.42.0"}, "needs --canary"},
		"the project as canary": {[]string{"deps", "bump", "golang.org/x/text", "0.42.0", "--canary", "."}, "not a leaf of the project"},
		"a folder outside":      {[]string{"deps", "bump", "golang.org/x/text", "0.42.0", "--canary", t.TempDir()}, "not inside the project"},
		"a package not there":   {[]string{"deps", "bump", "golang.org/x/other", "1.0.0", "--canary", "repos/api"}, "requires no package"},
		"a pre-release":         {[]string{"deps", "bump", "golang.org/x/text", "0.42.0-rc.1", "--canary", "repos/api"}, "pre-release"},
		"an option as package":  {[]string{"deps", "bump", "--", "-g", "0.42.0", "--canary", "repos/api"}, "not a package name"},
		"too few arguments":     {[]string{"deps", "bump", "golang.org/x/text", "--canary", "repos/api"}, "want 2 argument(s)"},
		"another subcommand":    {[]string{"deps", "audit"}, "usage:"},
	}
	for name, c := range cases {
		if code, out, errOut := ygg(t, append(c.args, common...)...); code != ExitRefused || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, want 2 and %q:\n%s%s", name, code, c.want, out, errOut)
		}
	}
	if mod(t, dir, "api") != goMod("0.41.0") || mod(t, dir, "old") != goMod("0.43.0") || bumpLedger(t, dir) != "" {
		t.Error("a refusal changed a file or wrote a ledger")
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory", "deps")); !os.IsNotExist(err) {
		t.Errorf("a refusal left the folder of a bump: %v", err)
	}
}

func TestDepsBumpRunsToolAndChecksInTheSandboxOfEachLeafAndStopsIt(t *testing.T) {
	dir, config := depsProject(t, goStack)
	fake := &sandboxfake.Sandbox{}
	old := newDepsSandbox
	newDepsSandbox = func(engine string, cfg sandbox.Config, work string) (gungnir.Sandbox, error) { return fake, nil }
	t.Cleanup(func() { newDepsSandbox = old })
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api", "--rollout", "repos/web")
	if code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if len(fake.Ups) != 2 || len(fake.Downs) != 2 {
		t.Fatalf("ups %d, downs %d", len(fake.Ups), len(fake.Downs))
	}
	up := fake.Ups[0]
	if up.Name != "repos/api" || up.Stack != goStack || strings.Join(up.Egress, ",") != "proxy.golang.org" || up.GitDir == "" || up.Leaf != filepath.Join(dir, "repos/api") || up.Scratch {
		t.Errorf("sandbox of the canary: %+v", up)
	}
	if fake.Ups[1].Name != "repos/web" || fake.Ups[1].Run != up.Run {
		t.Errorf("sandbox of the rollout product: %+v", fake.Ups[1])
	}
	var tools []string
	for _, c := range fake.Execs {
		tools = append(tools, strings.Join(c.Argv, " "))
	}
	if got := strings.Join(tools, "|"); got != "env GOWORK=off go get golang.org/x/text@v0.42.0|test -f go.mod|test -f ok.txt|env GOWORK=off go get golang.org/x/text@v0.42.0|test -f go.mod|test -f ok.txt" {
		t.Errorf("commands in the sandboxes: %s", got)
	}
}

// commitFixtures commits the manifest of each leaf, so that the bump finds the leaves clean.
func commitFixtures(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"api", "web", "old"} {
		ns, err := tree.Resolve(ctx, dir, "repos/"+name)
		if err != nil {
			t.Fatal(err)
		}
		repo := git.Repo{GitDir: ns[len(ns)-1].GitDir, WorkTree: ns[len(ns)-1].Dir}
		if _, err := repo.Run(ctx, "add", "go.mod", "ok.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Run(ctx, "commit", "-q", "-m", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDepsBumpRefusesALeafWhoseManifestHasUncommittedChanges(t *testing.T) {
	for _, file := range []string{"go.mod", "go.sum"} {
		dir, config := depsProject(t, "")
		p := filepath.Join(dir, "repos/web", file)
		if err := os.WriteFile(p, []byte(goMod("0.41.0")+"// dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host", "--canary", "repos/api", "--rollout", "repos/web")
		if code != ExitRefused || !strings.Contains(errOut, "uncommitted") || !strings.Contains(errOut, "commit or stash") {
			t.Errorf("%s: exit %d:\n%s%s", file, code, out, errOut)
		}
		if mod(t, dir, "api") != goMod("0.41.0") {
			t.Errorf("%s: the canary was touched", file)
		}
		if _, err := os.Stat(filepath.Join(dir, ".factory", "deps")); !os.IsNotExist(err) {
			t.Errorf("%s: a refusal left the folder of a bump: %v", file, err)
		}
	}
}

func TestDepsBumpWritesAStartedLineBeforeTheToolRuns(t *testing.T) {
	dir, config := depsProject(t, "")
	if code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host", "--canary", "repos/api"); code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	_, rest, _ := strings.Cut(bumpLedger(t, dir), "\n") // the first line is the host line
	first, _, _ := strings.Cut(rest, "\n")
	if !strings.Contains(first, `"event":"deps_bump"`) || !strings.Contains(first, "started") || strings.Contains(first, `"ygg.verdict"`) {
		t.Errorf("first line of the ledger: %s", first)
	}
}

func TestDepsBumpRefusalsWithASandboxStartNoSandboxAndLeaveNoTrace(t *testing.T) {
	dir, config := depsProject(t, goStack)
	started := 0
	old := newDepsSandbox
	newDepsSandbox = func(engine string, cfg sandbox.Config, work string) (gungnir.Sandbox, error) {
		started++
		return &sandboxfake.Sandbox{}, os.MkdirAll(filepath.Join(work, "registry-auth"), 0o755)
	}
	t.Cleanup(func() { newDepsSandbox = old })
	for name, args := range map[string][]string{
		"a bump down":    {"deps", "bump", "golang.org/x/text", "0.40.0", "--canary", "repos/api"},
		"a package none": {"deps", "bump", "golang.org/x/other", "1.0.0", "--canary", "repos/api"},
		"a rollout down": {"deps", "bump", "golang.org/x/text", "0.42.0", "--canary", "repos/api", "--rollout", "repos/old"},
	} {
		if code, out, errOut := ygg(t, append(args, "--project", dir, "--config", config)...); code != ExitRefused {
			t.Errorf("%s: exit %d:\n%s%s", name, code, out, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory", "deps")); !os.IsNotExist(err) || started != 0 {
		t.Errorf("a refusal started %d sandbox driver(s) or left the folder of a bump: %v", started, err)
	}
}

func TestDepsBumpFailsAToolThatPlantsAGitRepositoryUnderNodeModulesAndRemovesIt(t *testing.T) {
	depsFakeGoExtra = "mkdir -p node_modules/.c/.git && echo '[core]' > node_modules/.c/.git/config\n"
	t.Cleanup(func() { depsFakeGoExtra = "" })
	dir, config := depsProject(t, "")
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--host", "--canary", "repos/api", "--rollout", "repos/web")
	if code != ExitFailed || !strings.Contains(errOut, "repos/api failed") || !strings.Contains(out, "git repository") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if mod(t, dir, "api") != goMod("0.41.0") || mod(t, dir, "web") != goMod("0.41.0") {
		t.Error("a go.mod is not as it was")
	}
	if _, err := os.Stat(filepath.Join(dir, "repos/api/node_modules/.c/.git")); !os.IsNotExist(err) {
		t.Errorf("the planted repository is still there: %v", err)
	}
}

func TestDepsBumpWithNoSandboxAndNoHostIsRefusedBeforeTheLockAndLeavesNoTrace(t *testing.T) {
	dir, config := depsProject(t, "")
	release, err := lock.Take(run.RunsLock(dir)) // held: a refusal that came after the lock would say the project is busy
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api")
	want := "ygg: deps bump has no sandbox: factory.yaml has no sandbox section, so the package manager and its scripts would run on the host with the full network and your user id. Add a sandbox section, or pass --host to accept that\n"
	if code != ExitRefused || errOut != want {
		t.Errorf("exit %d, want %d and %q:\n%s%s", code, ExitRefused, want, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory", "deps")); !os.IsNotExist(err) {
		t.Errorf("a refusal left the folder of a bump: %v", err)
	}
	if mod(t, dir, "api") != goMod("0.41.0") {
		t.Error("a refusal changed the canary")
	}
}

func TestDepsBumpOnTheHostStartsItsLedgerWithTheHostLine(t *testing.T) {
	dir, config := depsProject(t, "")
	if code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api", "--host"); code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(bumpLedger(t, dir)), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], `"event":"deps_bump"`) || !strings.Contains(lines[0], deps.HostNote) || strings.Contains(lines[0], `"ygg.verdict"`) {
		t.Fatalf("the ledger does not start with the host line:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], "started") || strings.Contains(lines[1], `"ygg.verdict"`) {
		t.Errorf("second line of the ledger: %s", lines[1])
	}
	if got, err := deps.FindUnfinished(dir); err != nil || len(got) != 0 {
		t.Errorf("a finished host bump is unfinished: %+v, %v", got, err)
	}
}

func TestDepsBumpWithASandboxNeedsNoHostAndWritesNoHostLine(t *testing.T) {
	for _, extra := range [][]string{nil, {"--host"}} {
		dir, config := depsProject(t, goStack)
		fake := &sandboxfake.Sandbox{}
		old := newDepsSandbox
		newDepsSandbox = func(engine string, cfg sandbox.Config, work string) (gungnir.Sandbox, error) { return fake, nil }
		t.Cleanup(func() { newDepsSandbox = old })
		args := append([]string{"deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api"}, extra...)
		if code, out, errOut := ygg(t, args...); code != ExitDone {
			t.Fatalf("%v: exit %d:\n%s%s", extra, code, out, errOut)
		}
		if len(fake.Ups) != 1 {
			t.Errorf("%v: the bump did not run in the sandbox: %d ups", extra, len(fake.Ups))
		}
		if l := bumpLedger(t, dir); strings.Contains(l, deps.HostNote) {
			t.Errorf("%v: a sandbox bump has a host line:\n%s", extra, l)
		}
	}
}

func TestDepsBumpWhoseSandboxDoesNotStartIsNotReportedUnfinished(t *testing.T) {
	dir, config := depsProject(t, goStack)
	old := newDepsSandbox
	newDepsSandbox = func(string, sandbox.Config, string) (gungnir.Sandbox, error) { return nil, errors.New("no engine") }
	t.Cleanup(func() { newDepsSandbox = old })
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api")
	if code != ExitFailed {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if got, err := deps.FindUnfinished(dir); err != nil || len(got) != 0 {
		t.Errorf("found %+v, %v", got, err)
	}
}

// depsServices runs a bump of repos/api in a sandbox whose driver is a fake, and returns the fake, the exit
// code, the error output and how many drivers started.
func depsServices(t *testing.T, dir, config string) (fake *sandboxfake.Sandbox, code int, errOut string, started int) {
	t.Helper()
	fake = &sandboxfake.Sandbox{}
	old := newDepsSandbox
	newDepsSandbox = func(engine string, cfg sandbox.Config, work string) (gungnir.Sandbox, error) {
		started++
		return fake, nil
	}
	t.Cleanup(func() { newDepsSandbox = old })
	code, _, errOut = ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api")
	return fake, code, errOut, started
}

func TestDepsSandboxGetsTheRuntimesServices(t *testing.T) {
	dir, config := depsProjectWith(t, goStack, "needs: [postgres, s3]\n", true)
	fake, code, errOut, _ := depsServices(t, dir, config)
	if code != ExitDone || len(fake.Ups) != 1 {
		t.Fatalf("exit %d, %d sandboxes:\n%s", code, len(fake.Ups), errOut)
	}
	var got []string
	for _, v := range fake.Ups[0].Services {
		got = append(got, v.Name+"="+v.Image)
	}
	want := "postgres=" + pgImage + ",s3=docker.io/chrislusf/seaweedfs:4.45@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5"
	if strings.Join(got, ",") != want {
		t.Errorf("the sandbox started with the services %v; want the runtime realm's postgres and s3", got)
	}
}

func TestDepsRefusesAnUnmetNeed(t *testing.T) {
	dir, config := depsProjectWith(t, goStack, "needs: [postgres, s3]\n", false)
	_, code, errOut, started := depsServices(t, dir, config)
	if code != ExitRefused || started != 0 || !strings.Contains(errOut, "the realm helheim-go needs postgres") ||
		!strings.Contains(errOut, "the project pins no runtime realm to provide it") {
		t.Errorf("exit %d, %d drivers started; want a refusal with the message of services.For:\n%s", code, started, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory", "deps")); !os.IsNotExist(err) {
		t.Errorf("the refusal left the folder of a bump: %v", err)
	}
}

func TestDepsSandboxOfALeafThatNeedsNothingHasNoService(t *testing.T) {
	dir, config := depsProjectWith(t, goStack, "", true) // a runtime realm is pinned, and nothing asks for it
	fake, code, errOut, _ := depsServices(t, dir, config)
	if code != ExitDone || len(fake.Ups) != 1 || len(fake.Ups[0].Services) != 0 {
		t.Errorf("exit %d, sandboxes %+v; want one with no service:\n%s", code, fake.Ups, errOut)
	}
}

func TestDepsRefusesAPinThePackOfTheStoreDoesNotMatch(t *testing.T) {
	for name, c := range map[string]struct{ leaf, runtime, want string }{
		"the runtime pin": {"0.1.0", "0.9.0", "the realm store holds niflheim-local 0.1.0, but runtime pins 0.9.0"},
		"the leaf's pin":  {"0.9.0", "0.1.0", "the realm store holds helheim-go 0.1.0, but repos/api pins 0.9.0"},
	} {
		dir, config := depsProjectWith(t, goStack, "needs: [postgres]\n", true)
		u, err := schema.LoadUmbrella(dir)
		if err != nil {
			t.Fatal(err)
		}
		u.RuntimeVersion = c.runtime
		child := u.Children["repos/api"]
		child.RealmVersion = c.leaf
		u.Children["repos/api"] = child
		if err := u.Save(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := git.Run(ctx, dir, "commit", "-q", "-am", "the pins"); err != nil {
			t.Fatal(err)
		}
		_, code, errOut, started := depsServices(t, dir, config)
		if code != ExitRefused || started != 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, %d drivers started; want a refusal with %q:\n%s", name, code, started, c.want, errOut)
		}
	}
}
