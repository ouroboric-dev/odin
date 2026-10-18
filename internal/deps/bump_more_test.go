package deps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func TestARolloutStopsAtTheFirstFailureAndPutsBackOnlyThatProduct(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0", "r1": "0.41.0", "r2": "0.41.0", "r3": "0.41.0"})
	w.fail["r2:unit"] = 1
	before2, before3 := w.bytes("r2"), w.bytes("r3")
	rep, err := w.bumper().Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), []Product{w.product("r1"), w.product("r2"), w.product("r3")})
	if err != nil || !rep.Failed() || len(rep.Steps) != 3 {
		t.Fatalf("report %+v, error %v", rep, err)
	}
	if !same2(before2, w.bytes("r2")) || !same2(before3, w.bytes("r3")) || strings.Contains(strings.Join(w.opened, ","), "r3") {
		t.Errorf("r2 not restored or r3 touched; opened %v", w.opened)
	}
	if !strings.Contains(w.has("api"), "v0.42.0") || !strings.Contains(w.has("r1"), "v0.42.0") {
		t.Error("the products that passed were put back")
	}
	if got := strings.Join(w.eventLines(), "|"); !strings.HasSuffix(got, "deps_bump rollout fail golang.org/x/text 0.41.0>0.42.0") || strings.Count(got, "deps_bump") != 3 {
		t.Errorf("events: %s", got)
	}
}

func TestARefusalChangesNothingAndOpensNothing(t *testing.T) {
	cases := map[string]struct {
		pkg, version string
		canary       string
		rollout      []string
		mutate       func(*world)
		want         string
	}{
		"a bump down":          {"golang.org/x/text", "0.40.0", "api", nil, nil, "goes down"},
		"a rollout bump down":  {"golang.org/x/text", "0.42.0", "api", []string{"new"}, func(w *world) { write(w.t, w.root+"/new", "go.mod", goModAt("0.43.0")) }, "goes down"},
		"the canary has it":    {"golang.org/x/text", "0.41.0", "api", nil, nil, "nothing to bump"},
		"a pre-release":        {"golang.org/x/text", "0.42.0-rc.1", "api", nil, nil, "pre-release"},
		"a bad version":        {"golang.org/x/text", "latest", "api", nil, nil, "not a version"},
		"an option as package": {"--registry=http://evil", "1.0.0", "api", nil, nil, "not a package name"},
		"a package not there":  {"example.invalid/other", "1.0.0", "api", nil, nil, "requires no package"},
		"the canary twice":     {"golang.org/x/text", "0.42.0", "api", []string{"api"}, nil, "named twice"},
		"a missing rung":       {"golang.org/x/text", "0.42.0", "api", nil, nil, "defines no check unit"},
	}
	for name, c := range cases {
		w := newWorld(t, map[string]string{"api": "0.41.0"})
		if c.mutate != nil {
			c.mutate(w)
		}
		canary := w.product("api")
		if name == "a missing rung" {
			delete(canary.Checks, "unit")
		}
		var rollout []Product
		for _, r := range c.rollout {
			rollout = append(rollout, w.product(r))
		}
		before := w.bytes("api")
		_, err := w.bumper().Bump(context.Background(), c.pkg, c.version, canary, rollout)
		var refusal *Refusal
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want a refusal with %q", name, err, c.want)
		}
		if len(w.opened) != 0 || len(w.events) != 0 || !same2(before, w.bytes("api")) {
			t.Errorf("%s: it opened %v, logged %v or changed a file", name, w.opened, w.eventLines())
		}
	}
}

func TestAToolThatChangesAPolicyFileFailsTheProductAndTheFileIsPutBack(t *testing.T) {
	for _, file := range []string{"devcontainer/checks/config/deps-policy.json", "deps-policy.json", "main.go"} {
		w := newWorld(t, map[string]string{"api": "0.41.0"})
		w.also["api"] = [2]string{file, `{"schema":"ygg.deps/v1","minimum":{}}`}
		before := w.bytes("api")
		rep, err := w.bumper().Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), nil)
		if err != nil || !rep.Failed() || !strings.Contains(rep.Steps[0].Reason, file) {
			t.Fatalf("%s: report %+v, error %v", file, rep, err)
		}
		if !same2(before, w.bytes("api")) {
			t.Errorf("%s: not put back", file)
		}
		if strings.Contains(strings.Join(w.timeline, "|"), "api deps") {
			t.Errorf("%s: the ladder ran on a product the tool changed beyond its manifest", file)
		}
	}
}

func TestARolloutProductThatHasTheVersionIsSkippedWithoutBeingOpened(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0", "done": "0.42.0"})
	rep, err := w.bumper().Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), []Product{w.product("done")})
	if err != nil || rep.Failed() || rep.Steps[1].Verdict != "skip" || strings.Join(w.opened, ",") != "api" {
		t.Fatalf("report %+v, error %v, opened %v", rep, err, w.opened)
	}
	if len(w.events) != 2 { // deps_checked and the canary's deps_bump; the skipped product logs nothing
		t.Errorf("events %v", w.eventLines())
	}
}

func TestACancelledBumpPutsTheProductBackAndStopsItsSandbox(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0"})
	ctx, cancel := context.WithCancel(context.Background())
	w.onRun = func(_, check string) error {
		if check == "unit" {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	before := w.bytes("api")
	_, err := w.bumper().Bump(ctx, "golang.org/x/text", "0.42.0", w.product("api"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if !same2(before, w.bytes("api")) || strings.Join(w.stopped, ",") != "api" {
		t.Errorf("not put back, or not stopped: %v", w.stopped)
	}
	for _, e := range w.events {
		if e.Name == gungnir.EventDepsBump {
			t.Errorf("a cancelled bump logged a verdict: %+v", e)
		}
	}
}

func TestNpmBumpKeepsTheTildeAndRunsNoScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"react":"~19.1.0"}}`)
	write(t, dir, "package-lock.json", "{}\n")
	var got []string
	w := &npmWorld{dir: dir, got: &got}
	b := &Bumper{Open: w, LogDir: filepath.Join(dir, "..", "logs"), Log: func(gungnir.Event) error { return nil }}
	p := Product{Name: "web", Dir: dir, Realm: "midgard-react", Checks: map[string][]string{"deps": {"web-deps.sh"}, "unit": {"web-unit.sh"}}}
	rep, err := b.Bump(context.Background(), "react", "19.1.4", p, nil)
	if err != nil || rep.Failed() {
		t.Fatalf("report %+v, error %v", rep, err)
	}
	if got[0] != "npm install --ignore-scripts --save-prefix=~ react@19.1.4" {
		t.Errorf("tool command %q", got[0])
	}
}

type npmWorld struct {
	dir string
	got *[]string
}

func (n *npmWorld) Open(context.Context, Product) (gungnir.Runner, func(context.Context) error, error) {
	return n, func(context.Context) error { return nil }, nil
}

func (n *npmWorld) Run(_ context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	*n.got = append(*n.got, strings.Join(c.Argv, " "))
	if c.Argv[0] == "npm" {
		os.WriteFile(filepath.Join(n.dir, "package.json"), []byte(`{"dependencies":{"react":"~19.1.4"}}`), 0o644)
		os.WriteFile(filepath.Join(n.dir, "package-lock.json"), []byte("{\"v\":2}\n"), 0o644)
	}
	return gungnir.ExecResult{}, nil
}
