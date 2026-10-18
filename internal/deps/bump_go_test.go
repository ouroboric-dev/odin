package deps

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

// The tests below run the real go tool, with no network: the module comes from a module proxy that is a
// folder (GOPROXY=file://...), built here. Version 1.0.1 of the module is compatible with the product's
// code; 1.1.0 removes a function the code calls, so the product's build, which is its unit rung here, fails.

func writeProxy(t *testing.T) string {
	t.Helper()
	proxy := t.TempDir()
	versions := map[string]string{
		"v1.0.0": "package dep\n\nfunc F() string { return \"one\" }\n",
		"v1.0.1": "package dep\n\nfunc F() string { return \"one, patched\" }\n",
		"v1.1.0": "package dep\n\nfunc G() string { return \"F is gone\" }\n",
	}
	dir := filepath.Join(proxy, "example.invalid", "dep", "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var list string
	for v, src := range versions {
		list += v + "\n"
		mod := "module example.invalid/dep\n\ngo 1.21\n"
		must := func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		}
		must(os.WriteFile(filepath.Join(dir, v+".mod"), []byte(mod), 0o644))
		must(os.WriteFile(filepath.Join(dir, v+".info"), []byte(fmt.Sprintf(`{"Version":%q,"Time":"2026-01-01T00:00:00Z"}`, v)), 0o644))
		f, err := os.Create(filepath.Join(dir, v+".zip"))
		must(err)
		zw := zip.NewWriter(f)
		for name, body := range map[string]string{"go.mod": mod, "dep.go": src} {
			w, err := zw.Create("example.invalid/dep@" + v + "/" + name)
			must(err)
			_, err = w.Write([]byte(body))
			must(err)
		}
		must(zw.Close())
		must(f.Close())
	}
	if err := os.WriteFile(filepath.Join(dir, "list"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	return proxy
}

// goRunner runs commands with the real tools on the host, with no network and no cache of the operator's.
type goRunner struct {
	dir string
	env []string
}

func (g goRunner) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = g.dir, g.env, c.Stdout, c.Stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return gungnir.ExecResult{ExitCode: ee.ExitCode()}, nil
	}
	return gungnir.ExecResult{}, err
}

type goOpener struct{ env []string }

func (o goOpener) Open(_ context.Context, p Product) (gungnir.Runner, func(context.Context) error, error) {
	return goRunner{p.Dir, o.env}, func(context.Context) error { return nil }, nil
}

func goProduct(t *testing.T, env []string, name string) Product {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	write(t, dir, "go.mod", "module example.invalid/"+name+"\n\ngo 1.21\n\nrequire example.invalid/dep v1.0.0\n")
	write(t, dir, "main.go", "package main\n\nimport \"example.invalid/dep\"\n\nfunc main() { println(dep.F()) }\n")
	write(t, dir, "devcontainer/checks/config/deps-policy.json", policyJSON)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir, tidy.Env = dir, env
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return Product{Name: name, Dir: dir, Realm: "helheim-go", Checks: map[string][]string{
		"deps": {"go", "mod", "verify"}, "unit": {"go", "build", "./..."}}}
}

func goWorld(t *testing.T) (env []string, b *Bumper, events *[]gungnir.Event) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool on PATH")
	}
	proxy, tmp := writeProxy(t), t.TempDir()
	env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "GOPATH=" + filepath.Join(tmp, "gopath"), "GOCACHE=" + filepath.Join(tmp, "cache"),
		"GOMODCACHE=" + filepath.Join(tmp, "mod"), "GOFLAGS=-modcacherw", "GOPROXY=file://" + proxy, "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off"}
	events = &[]gungnir.Event{}
	return env, &Bumper{Open: goOpener{env}, LogDir: filepath.Join(tmp, "logs"), Log: func(e gungnir.Event) error {
		*events = append(*events, e)
		return nil
	}}, events
}

func TestTheRealGoBumpsACanaryAndThenTheRolloutProductOffline(t *testing.T) {
	env, b, events := goWorld(t)
	canary, next := goProduct(t, env, "canary"), goProduct(t, env, "next")
	rep, err := b.Bump(context.Background(), "example.invalid/dep", "1.0.1", canary, []Product{next})
	if err != nil || rep.Failed() || len(rep.Steps) != 2 {
		t.Fatalf("report %+v, error %v", rep, err)
	}
	for _, p := range []Product{canary, next} {
		if m := mustFind(t, p.Dir, "example.invalid/dep"); m.Current.String() != "1.0.1" {
			t.Errorf("%s requires %s", p.Name, m.Current)
		}
	}
	if len(*events) != 4 || (*events)[1].Verdict != "pass" || (*events)[3].Stage != "rollout" {
		t.Errorf("events %+v", *events)
	}
}

func TestTheRealGoCanaryThatBreaksTheBuildIsPutBackAndTheRolloutProductIsNotTouched(t *testing.T) {
	env, b, events := goWorld(t)
	canary, next := goProduct(t, env, "canary"), goProduct(t, env, "next")
	before := map[string]string{}
	for _, p := range []Product{canary, next} {
		for k, v := range (&world{root: filepath.Dir(p.Dir)}).bytes(p.Name) {
			before[k] = v
		}
	}
	rep, err := b.Bump(context.Background(), "example.invalid/dep", "1.1.0", canary, []Product{next})
	if err != nil || !rep.Failed() || len(rep.Steps) != 1 || !strings.Contains(rep.Steps[0].Reason, "check unit failed") {
		t.Fatalf("report %+v, error %v", rep, err)
	}
	after := map[string]string{}
	for _, p := range []Product{canary, next} {
		for k, v := range (&world{root: filepath.Dir(p.Dir)}).bytes(p.Name) {
			after[k] = v
		}
	}
	if !same2(before, after) {
		t.Error("a file of the canary or of the rollout product is not as it was")
	}
	if n := len(*events); n != 2 || (*events)[1].Stage != "canary" || (*events)[1].Verdict != "fail" {
		t.Errorf("events %+v", *events)
	}
}
