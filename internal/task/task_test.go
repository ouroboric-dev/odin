package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

const brief = "# Tiny\n\nBuild a tiny product.\n"

const manifest = `{"schema":"ygg.suite/v1","suite":"v1","scenarios":[
 {"id":"a thing works","category":"core","kind":"fail-to-pass"},
 {"id":"a thing stays","category":"core","kind":"pass-to-pass"},
 {"id":"a thing is private","category":"security","kind":"fail-to-pass"}]}`

func taskToml(sha string) string {
	return `schema_version = "1.4"
artifacts = ["/app"]

[task]
name = "ouroboric-dev/tiny-task"
version = "2.1.0"

[metadata]
brief_sha256 = "` + sha + `"
suite = "v1"

[verifier]
timeout_sec = 300.0
environment_mode = "separate"

[verifier.env]
WORKERS = "2"
SCORE_WEIGHTS = "core=0.6,security=0.4"
`
}

func sha(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }

// fake is a tiny task and a tiny suite, written by the test: tasks/<id> is the public half, suites/<id>/v1
// the hidden half. Nothing here is a real suite.
type fake struct{ tasks, suites, dir, tests string }

func newFake(t *testing.T) *fake {
	t.Helper()
	root := t.TempDir()
	f := &fake{tasks: filepath.Join(root, "tasks"), suites: filepath.Join(root, "suites")}
	f.dir = filepath.Join(f.tasks, "tiny-task")
	f.tests = filepath.Join(f.suites, "tiny-task", "v1", "tests")
	f.write(t, "task.toml", taskToml(sha(brief)))
	f.write(t, "instruction.md", brief)
	f.write(t, "environment/Dockerfile", "FROM scratch\n")
	f.writeAt(t, filepath.Join(f.tests, "test.sh"), "#!/bin/bash\nexit 0\n")
	f.writeAt(t, filepath.Join(f.tests, "manifest.json"), manifest)
	return f
}

func (f *fake) write(t *testing.T, rel, body string) {
	t.Helper()
	f.writeAt(t, filepath.Join(f.dir, rel), body)
}

func (f *fake) writeAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fake) load() (*Task, error) { return Load(f.tasks, f.suites, "tiny-task") }

func wantRefusal(t *testing.T, err error, reason gungnir.GradeReason, detail string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("Load() = %v, want a *Refusal (%s)", err, reason)
	}
	if r.Reason != reason || !strings.Contains(r.Detail, detail) {
		t.Errorf("refusal = %s: %q, want %s with %q", r.Reason, r.Detail, reason, detail)
	}
}

func TestLoadReadsTheTaskAndFindsItsSuite(t *testing.T) {
	f := newFake(t)
	tk, err := f.load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if tk.ID != "tiny-task" || tk.Version != "2.1.0" || tk.Suite != "v1" || tk.BriefSHA256 != sha(brief) {
		t.Errorf("task = %+v", tk)
	}
	if tk.Environment != filepath.Join(f.dir, "environment") || tk.Tests != f.tests {
		t.Errorf("folders = %s, %s", tk.Environment, tk.Tests)
	}
	if tk.Timeout != 300*time.Second || tk.Env["WORKERS"] != "2" || tk.Weights["core"] != 0.6 || tk.Weights["security"] != 0.4 {
		t.Errorf("limits = %v, env %v, weights %v", tk.Timeout, tk.Env, tk.Weights)
	}
	if len(tk.Manifest.Scenarios) != 3 || tk.Manifest.Suite != "v1" {
		t.Errorf("manifest = %+v", tk.Manifest)
	}
}

func TestGraderRefusesRetiredVersion(t *testing.T) {
	f := newFake(t)
	// A retired suite is a file or a folder named for it; the suite itself may still be in the store.
	f.write(t, "retired/v1", "retired after the proof runs\n")
	_, err := f.load()
	wantRefusal(t, err, gungnir.GradeRetired, "v1")
	os.Remove(filepath.Join(f.dir, "retired", "v1"))
	f.write(t, "retired/v1/README.md", "folder form\n")
	_, err = f.load()
	wantRefusal(t, err, gungnir.GradeRetired, "v1")
	// Another version's mark does not retire this one.
	os.RemoveAll(filepath.Join(f.dir, "retired"))
	f.write(t, "retired/v0", "old\n")
	if _, err := f.load(); err != nil {
		t.Errorf("Load() with only v0 retired = %v", err)
	}
}

func TestAChangedBriefIsRefused(t *testing.T) {
	f := newFake(t)
	f.write(t, "instruction.md", brief+"One more line.\n")
	_, err := f.load()
	wantRefusal(t, err, gungnir.GradeBriefChanged, sha(brief)[:12])
}

func TestAMissingSuiteIsRefused(t *testing.T) {
	cases := map[string]func(f *fake){
		"no store entry for the task": func(f *fake) { os.RemoveAll(filepath.Join(f.suites, "tiny-task")) },
		"no tests folder":             func(f *fake) { os.RemoveAll(f.tests) },
		"no test.sh":                  func(f *fake) { os.Remove(filepath.Join(f.tests, "test.sh")) },
		"no manifest":                 func(f *fake) { os.Remove(filepath.Join(f.tests, "manifest.json")) },
	}
	for name, mutate := range cases {
		f := newFake(t)
		mutate(f)
		_, err := f.load()
		t.Run(name, func(t *testing.T) { wantRefusal(t, err, gungnir.GradeMissingSuite, "v1") })
	}
}

func TestATaskFileThatIsNotExactIsRefused(t *testing.T) {
	good := taskToml(sha(brief))
	cases := map[string]struct{ from, to, want string }{
		"a verifier on the same machine": {`"separate"`, `"shared"`, "environment_mode"},
		"no version":                     {`version = "2.1.0"`, ``, "task.version"},
		"a version that is not x.y.z":    {`"2.1.0"`, `"2.1"`, "task.version"},
		"a name that is not the folder":  {`ouroboric-dev/tiny-task`, `ouroboric-dev/other`, "task.name"},
		"a brief hash that is not hex":   {sha(brief), `xyz`, "brief_sha256"},
		"a suite that is a path":         {`suite = "v1"`, `suite = "../v1"`, "metadata.suite"},
		"no suite":                       {`suite = "v1"`, ``, "metadata.suite"},
		"more than /app as artifact":     {`["/app"]`, `["/app", "/etc"]`, "artifacts"},
		"weights that sum to 1.1":        {`core=0.6,security=0.4`, `core=0.7,security=0.4`, "SCORE_WEIGHTS"},
		"a weight twice":                 {`core=0.6,security=0.4`, `core=0.6,core=0.4`, "SCORE_WEIGHTS"},
		"a weight that is not a number":  {`core=0.6,security=0.4`, `core=0.6,security=lots`, "SCORE_WEIGHTS"},
		"no weights":                     {`SCORE_WEIGHTS = "core=0.6,security=0.4"`, ``, "SCORE_WEIGHTS"},
		"no timeout":                     {`timeout_sec = 300.0`, ``, "timeout_sec"},
		"a lower case env name":          {`WORKERS = "2"`, `workers = "2"`, "verifier.env"},
		"toml that does not parse":       {`[task]`, `[task`, "task.toml"},
	}
	for name, c := range cases {
		f := newFake(t)
		if !strings.Contains(good, c.from) {
			t.Fatalf("%s: the good file lacks %q", name, c.from)
		}
		f.write(t, "task.toml", strings.Replace(good, c.from, c.to, 1))
		_, err := f.load()
		t.Run(name, func(t *testing.T) { wantRefusal(t, err, gungnir.GradeBadTask, c.want) })
	}
	f := newFake(t)
	os.Remove(filepath.Join(f.dir, "task.toml"))
	_, err := f.load()
	wantRefusal(t, err, gungnir.GradeBadTask, "task.toml")
	for _, id := range []string{"", "../tiny-task", "Tiny", "tiny/task", "-tiny"} {
		_, err := Load(f.tasks, f.suites, id)
		wantRefusal(t, err, gungnir.GradeBadTask, "task id")
	}
}

func TestATaskFolderMayNotLeadOutsideItself(t *testing.T) {
	f := newFake(t)
	outside := filepath.Join(t.TempDir(), "instruction.md")
	os.WriteFile(outside, []byte(brief), 0o644)
	os.Remove(filepath.Join(f.dir, "instruction.md"))
	if err := os.Symlink(outside, filepath.Join(f.dir, "instruction.md")); err != nil {
		t.Skip("no symlinks here")
	}
	_, err := f.load()
	wantRefusal(t, err, gungnir.GradeBadTask, "instruction.md")
}

func TestAManifestThatCannotBeReadExactlyIsRefused(t *testing.T) {
	cases := map[string]string{
		"not json":                  `{"schema":`,
		"an unknown field":          strings.Replace(manifest, `"kind":"fail-to-pass"}]`, `"kind":"fail-to-pass","skip":true}]`, 1),
		"another schema":            strings.Replace(manifest, "ygg.suite/v1", "ygg.suite/v9", 1),
		"another suite":             strings.Replace(manifest, `"suite":"v1"`, `"suite":"v2"`, 1),
		"an id twice":               strings.Replace(manifest, "a thing stays", "a thing works", 1),
		"no scenarios":              `{"schema":"ygg.suite/v1","suite":"v1","scenarios":[]}`,
		"a category with no weight": strings.Replace(manifest, `"security"`, `"billing"`, 1),
		"a second document":         manifest + manifest,
	}
	for name, body := range cases {
		f := newFake(t)
		f.writeAt(t, filepath.Join(f.tests, "manifest.json"), body)
		_, err := f.load()
		t.Run(name, func(t *testing.T) { wantRefusal(t, err, gungnir.GradeBadManifest, "") })
	}
}

// The smoke task of mimir is the real shape. Its sample suite is PUBLIC, so it may stand in for a hidden
// suite here. The test needs a checkout of mimir: YGG_MIMIR names it, else it is looked for beside odin.
func TestTheSmokeTaskOfMimirLoadsWithItsSampleSuite(t *testing.T) {
	mimir := os.Getenv("YGG_MIMIR")
	if mimir == "" {
		mimir = filepath.Join("..", "..", "..", "mimir")
	}
	tests := filepath.Join(mimir, "smoke-notes", "sample-suite", "tests")
	if _, err := os.Stat(tests); err != nil {
		if os.Getenv("YGG_REQUIRE_MIMIR") != "" {
			t.Fatal(err)
		}
		t.Skip("no checkout of mimir: set YGG_MIMIR")
	}
	suites := t.TempDir()
	if err := os.CopyFS(filepath.Join(suites, "smoke-notes", "v1", "tests"), os.DirFS(tests)); err != nil {
		t.Fatal(err)
	}
	tk, err := Load(mimir, suites, "smoke-notes")
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if tk.Version != "1.0.0" || tk.Suite != "v1" || len(tk.Manifest.Scenarios) != 7 || tk.Weights["security"] != 0.4 {
		t.Errorf("task = %+v", tk)
	}
}
