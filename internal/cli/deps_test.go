package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/ledger"
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
	if err := os.MkdirAll(filepath.Join(store, "helheim-go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "helheim-go", "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := treetest.Pin{Realm: "helheim-go", Version: "0.1.0"}
	leaf := treetest.Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate, Leaf: true, Pin: pin}
	dir = treetest.Build(t, treetest.Node{Owner: "ouroboric-dev", Visibility: gungnir.VisibilityPrivate,
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
	extra := "realms:\n  dir: " + store + "\n"
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

func TestDepsBumpMovesTheCanaryThenTheRolloutProduct(t *testing.T) {
	dir, config := depsProject(t, "")
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config,
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
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api", "--rollout", "repos/web")
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
	common := []string{"--project", dir, "--config", config}
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
		code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api", "--rollout", "repos/web")
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
	if code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api"); code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	first, _, _ := strings.Cut(bumpLedger(t, dir), "\n")
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
	code, out, errOut := ygg(t, "deps", "bump", "golang.org/x/text", "0.42.0", "--project", dir, "--config", config, "--canary", "repos/api", "--rollout", "repos/web")
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
