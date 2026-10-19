package deps

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

const policyJSON = `{"schema":"ygg.deps/v1","pin":"minor","minimum":{"golang.org/x/text":"0.42.0"},"audit":["govulncheck","osv"]}`

func goModAt(v string) string {
	return "module example.invalid/app\r\n\r\ngo 1.26\r\n\r\nrequire golang.org/x/text v" + v + "\r\n"
}

// world is a project of products, a fake that stands for the sandbox and the tools in it, and a timeline of
// what happened, in order, across commands and ledger lines.
type world struct {
	t        *testing.T
	root     string
	timeline []string
	events   []gungnir.Event
	opened   []string
	stopped  []string
	// fail maps "product:check" (check is bump, deps or unit) to an exit code.
	fail map[string]int
	// also is an extra change the tool makes in a product, besides the manifest: file, then text.
	also map[string][2]string
	// during is a change a check makes when it runs, by "product:check": file, then text.
	during map[string][2]string
	// onRun, if set, sees each command before it is answered.
	onRun func(product, check string) error
}

func newWorld(t *testing.T, versions map[string]string) *world {
	w := &world{t: t, root: t.TempDir(), fail: map[string]int{}, also: map[string][2]string{}, during: map[string][2]string{}}
	for name, v := range versions {
		write(t, filepath.Join(w.root, name), "go.mod", goModAt(v))
		write(t, filepath.Join(w.root, name), "go.sum", "golang.org/x/text v"+v+" h1:aaa=\r\n")
		write(t, filepath.Join(w.root, name), "main.go", "package main\n")
		write(t, filepath.Join(w.root, name), "devcontainer/checks/config/deps-policy.json", policyJSON)
	}
	return w
}

func (w *world) product(name string) Product {
	return Product{Name: name, Dir: filepath.Join(w.root, name), Realm: "helheim-go", Checks: map[string][]string{
		"deps": {"/opt/ygg/checks/go-deps.sh"}, "unit": {"/opt/ygg/checks/go-unit.sh"}, "static": {"/opt/ygg/checks/go-static.sh"}}}
}

func (w *world) bumper() *Bumper {
	return &Bumper{Open: w, LogDir: filepath.Join(w.root, "logs"), Log: func(e gungnir.Event) error {
		w.events = append(w.events, e)
		w.timeline = append(w.timeline, "event "+string(e.Name)+" "+e.Stage+" "+e.Verdict)
		return nil
	}}
}

func (w *world) Open(_ context.Context, p Product) (gungnir.Runner, func(context.Context) error, error) {
	w.opened = append(w.opened, p.Name)
	return fakeRunner{w, p}, func(context.Context) error { w.stopped = append(w.stopped, p.Name); return nil }, nil
}

type fakeRunner struct {
	w *world
	p Product
}

var toolVersion = regexp.MustCompile(`@v?([0-9][^ ]*)$`)

func (f fakeRunner) Run(_ context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	w, p := f.w, f.p
	check := "bump"
	switch {
	case strings.HasSuffix(c.Argv[0], "go-deps.sh"):
		check = "deps"
	case strings.HasSuffix(c.Argv[0], "go-unit.sh"):
		check = "unit"
	}
	w.timeline = append(w.timeline, p.Name+" "+check)
	if w.onRun != nil {
		if err := w.onRun(p.Name, check); err != nil {
			return gungnir.ExecResult{}, err
		}
	}
	if x, ok := w.during[p.Name+":"+check]; ok {
		write(w.t, p.Dir, x[0], x[1])
	}
	if code := w.fail[p.Name+":"+check]; code != 0 {
		c.Stdout.Write([]byte(check + " says no\n"))
		return gungnir.ExecResult{ExitCode: code}, nil
	}
	if check == "bump" { // what go get does: the version in go.mod and go.sum
		v := toolVersion.FindStringSubmatch(c.Argv[len(c.Argv)-1])[1]
		write(w.t, p.Dir, "go.mod", goModAt(v))
		write(w.t, p.Dir, "go.sum", "golang.org/x/text v"+v+" h1:bbb=\r\n")
		if x, ok := w.also[p.Name]; ok {
			write(w.t, p.Dir, x[0], x[1])
		}
	}
	return gungnir.ExecResult{}, nil
}

// bytes reads every file of a product, so a test can say "as it was".
func (w *world) bytes(name string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(filepath.Join(w.root, name), func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func same2(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (w *world) eventLines() []string {
	var out []string
	for _, e := range w.events {
		out = append(out, string(e.Name)+" "+e.Stage+" "+e.Verdict+" "+e.Package+" "+e.From+">"+e.To)
	}
	return out
}

func (w *world) has(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.root, name, "go.mod"))
	return string(b)
}

func TestACanaryThatPassesRollsOutOneProductAtATime(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0", "api2": "0.41.0", "api3": "0.40.0"})
	rep, err := w.bumper().Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), []Product{w.product("api2"), w.product("api3")})
	if err != nil || rep.Failed() || len(rep.Steps) != 3 {
		t.Fatalf("report %+v, error %v", rep, err)
	}
	want := []string{
		"event deps_bump canary ", "api bump", "api deps", "event deps_checked  pass", "api unit", "event deps_bump canary pass",
		"event deps_bump rollout ", "api2 bump", "api2 deps", "event deps_checked  pass", "api2 unit", "event deps_bump rollout pass",
		"event deps_bump rollout ", "api3 bump", "api3 deps", "event deps_checked  pass", "api3 unit", "event deps_bump rollout pass",
	}
	if got := strings.Join(w.timeline, "|"); got != strings.Join(want, "|") {
		t.Errorf("timeline\n got %s\nwant %s", got, strings.Join(want, "|"))
	}
	for _, n := range []string{"api", "api2", "api3"} {
		if !strings.Contains(w.has(n), "v0.42.0") {
			t.Errorf("%s: %q", n, w.has(n))
		}
	}
	if w.events[2].From != "0.41.0" || w.events[2].To != "0.42.0" || w.events[2].Package != "golang.org/x/text" || w.events[len(w.events)-1].From != "0.40.0" {
		t.Errorf("events: %v", w.eventLines())
	}
	if strings.Join(w.stopped, ",") != "api,api2,api3" {
		t.Errorf("stopped %v", w.stopped)
	}
}

func TestACanaryThatFailsIsPutBackByteForByteAndNoRolloutProductIsTouched(t *testing.T) {
	for _, bad := range []string{"api:bump", "api:deps", "api:unit"} {
		w := newWorld(t, map[string]string{"api": "0.41.0", "api2": "0.41.0"})
		w.fail[bad] = 1
		beforeCanary, beforeRollout := w.bytes("api"), w.bytes("api2")
		rep, err := w.bumper().Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), []Product{w.product("api2")})
		if err != nil || !rep.Failed() || len(rep.Steps) != 1 || rep.Steps[0].Stage != "canary" {
			t.Fatalf("%s: report %+v, error %v", bad, rep, err)
		}
		if !same2(beforeCanary, w.bytes("api")) {
			t.Errorf("%s: the canary is not as it was", bad)
		}
		if !same2(beforeRollout, w.bytes("api2")) || strings.Join(w.opened, ",") != "api" {
			t.Errorf("%s: a rollout product was touched or opened: %v", bad, w.opened)
		}
		last := w.events[len(w.events)-1]
		if last.Name != gungnir.EventDepsBump || last.Stage != "canary" || last.Verdict != "fail" || !strings.Contains(last.Note, "api") {
			t.Errorf("%s: last event %+v", bad, last)
		}
		for _, e := range w.events {
			if e.Stage == "rollout" {
				t.Errorf("%s: a rollout event: %+v", bad, e)
			}
		}
		if strings.Join(w.stopped, ",") != "api" {
			t.Errorf("%s: the canary's sandbox was not stopped: %v", bad, w.stopped)
		}
	}
}
