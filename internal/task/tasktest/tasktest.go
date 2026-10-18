// Package tasktest writes a tiny benchmark task and a tiny stand-in for its hidden suite into a
// temporary folder, for tests of the grader. The suite is made up: it holds no real scenario.
package tasktest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Brief is the text of the task's instruction.md.
const Brief = "# Tiny\n\nBuild a tiny product.\n"

// Manifest is the stand-in suite's manifest: two fail-to-pass scenarios in core, one in security, one
// pass-to-pass in core. Weights are core 0.6 and security 0.4.
const Manifest = `{"schema":"ygg.suite/v1","suite":"v1","scenarios":[
 {"id":"a thing works","category":"core","kind":"fail-to-pass"},
 {"id":"a thing stays","category":"core","kind":"pass-to-pass"},
 {"id":"a second thing works","category":"core","kind":"fail-to-pass"},
 {"id":"a thing is private","category":"security","kind":"fail-to-pass"}]}`

// Titles are the scenario ids of Manifest, in its order.
var Titles = []string{"a thing works", "a thing stays", "a second thing works", "a thing is private"}

// Task is a written task: TasksDir/tiny-task is its public half, SuiteDir/tiny-task/v1 the stand-in
// for the hidden half. The two folders are apart, as the factory config demands.
type Task struct {
	ID       string
	TasksDir string
	SuiteDir string
	Dir      string // TasksDir/tiny-task
	Tests    string // SuiteDir/tiny-task/v1/tests
}

// Brief returns the path of the task's instruction.md, which a run takes as its brief.
func (k *Task) Brief() string { return filepath.Join(k.Dir, "instruction.md") }

// New writes the task and the stand-in suite into a temporary folder of t.
func New(t *testing.T) *Task {
	t.Helper()
	root := t.TempDir()
	k := &Task{ID: "tiny-task", TasksDir: filepath.Join(root, "tasks"), SuiteDir: filepath.Join(root, "suites")}
	k.Dir = filepath.Join(k.TasksDir, k.ID)
	k.Tests = filepath.Join(k.SuiteDir, k.ID, "v1", "tests")
	sum := sha256.Sum256([]byte(Brief))
	toml := "artifacts = [\"/app\"]\n\n[task]\nname = \"ouroboric-dev/tiny-task\"\nversion = \"2.1.0\"\n\n" +
		"[metadata]\nbrief_sha256 = \"" + hex.EncodeToString(sum[:]) + "\"\nsuite = \"v1\"\n\n" +
		"[verifier]\ntimeout_sec = 300.0\nenvironment_mode = \"separate\"\n\n[verifier.env]\nWORKERS = \"2\"\nSCORE_WEIGHTS = \"core=0.6,security=0.4\"\n"
	for p, body := range map[string]string{
		filepath.Join(k.Dir, "task.toml"):                 toml,
		filepath.Join(k.Dir, "instruction.md"):            Brief,
		filepath.Join(k.Dir, "environment", "Dockerfile"): "FROM scratch\n",
		filepath.Join(k.Tests, "test.sh"):                 "#!/bin/bash\nexit 0\n",
		filepath.Join(k.Tests, "manifest.json"):           Manifest,
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// Results is a Playwright report for the stand-in suite: one spec for each title, with the status given
// for it ("passed" when the title is not in status).
func Results(status map[string]string) string {
	out := `{"suites":[{"title":"stand-in.spec.ts","specs":[`
	for i, title := range Titles {
		st := status[title]
		if st == "" {
			st = "passed"
		}
		if i > 0 {
			out += ","
		}
		out += `{"title":"` + title + `","tests":[{"results":[{"status":"` + st + `"}]}]}`
	}
	return out + `]}]}`
}
