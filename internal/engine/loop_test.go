package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
)

const loopNode = "  - { id: build, kind: loop, repo: repos/app, role: planner, prompt: p.md, attempts: 3, checks: [unit], input: [rejection, refusal], output: ygg.implement/v1 }\n"
const unitCheck = "checks:\n  unit: [grep, -q, done, build.txt]\n"

func write(path, content string) string {
	return "{ write: { path: " + path + ", content: \"" + content + "\\n\" } }"
}

func startLoop(t *testing.T, mode, script string) *run.Run {
	t.Helper()
	return startWith(t, setup{mode: mode, pack: unitCheck, nodes: loopNode, script: script})
}

func transcript(t *testing.T, r *run.Run, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, "transcripts", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestALoopWhoseWorkPassesItsChecksIsOneAttempt(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, summary: built it, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n"
	r := startLoop(t, "key-points", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	rec := r.State.Loops["build"]
	if rec == nil || !rec.State.Passed || rec.State.Attempt != 1 || r.State.Attempts["build"] != 1 {
		t.Errorf("loop record = %+v, sessions %d", rec, r.State.Attempts["build"])
	}
	if got := r.State.Summaries["build"]; !strings.Contains(got, "passed in attempt 1 of 3") {
		t.Errorf("summary = %q", got)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"loop_attempt"`, "attempt 1 of 3 starts with 0 finding(s) open", `"event":"check_result"`, "unit: exit 0", `"ygg.result":"passed"`, `"event":"node_finished"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	l, _ := r.Leaf("repos/app")
	if head, _ := l.Head(context.Background()); head != r.State.Leaves["repos/app"].Checkpoint {
		t.Error("the attempt's work is not committed as the leaf's checkpoint")
	}
}

func TestAFailingCheckGoesBackToTheNextAttemptAsAFinding(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, summary: first try, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, summary: second try, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n"
	r := startLoop(t, "key-points", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Attempts["build"] != 2 || r.State.Loops["build"].State.Attempt != 2 {
		t.Errorf("sessions %d, loop attempt %d; want 2 and 2", r.State.Attempts["build"], r.State.Loops["build"].State.Attempt)
	}
	first, second := promptOf(t, r, "build-1"), promptOf(t, r, "build-2")
	if !strings.Contains(first, "## Input: open_findings\n\nNone.\n") {
		t.Errorf("the first attempt was told of findings:\n%s", first)
	}
	for _, want := range []string{"## Input: open_findings\n\nThe checks and reviewers found these problems", `"id": "check-unit"`, `"severity": "blocker"`, `"source": "unit"`,
		`"location": "logs/build-1-unit.log:1"`, "check unit failed: exit status 1", "Do not say any of them is fixed",
		"## Input: check_results", `"check": "unit"`, `"passed": false`} {
		if !strings.Contains(second, want) {
			t.Errorf("the second attempt's prompt lacks %q:\n%s", want, second)
		}
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"finding_raised"`, `"ygg.finding.id":"check-unit"`, `"ygg.finding.severity":"blocker"`,
		`"event":"finding_closed"`, `"ygg.finding.attempts":2`, "unit: exit status 1", "unit: exit 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	// The check wrote nothing that stays: the leaf is at the commit the checks ran on.
	l, _ := r.Leaf("repos/app")
	if st, _ := l.Git(context.Background(), "status", "--porcelain"); st != "" {
		t.Errorf("the leaf holds %q after the checks", st)
	}
}

func TestASessionThatFailsAsAnAgentUsesUpAnAttemptAndLeavesNoHalfWork(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("junk.txt", "half a thought") + " ]\n      error: the tests hung and the session timed out\n      usage: { in: 50, out: 5, turns: 1 }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, summary: built it, output: '{\"summary\": \"done\"}' }\n"
	r := startLoop(t, "unattended", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if r.State.Loops["build"].State.Attempt != 2 {
		t.Errorf("the loop is in attempt %d; the failed session was an attempt", r.State.Loops["build"].State.Attempt)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "junk.txt")); err == nil {
		t.Error("what the failed session wrote is still in the leaf")
	}
	second := transcript(t, r, "build-2")
	if !strings.Contains(second, "## The last attempt failed") || !strings.Contains(second, "the tests hung and the session timed out") {
		t.Errorf("attempt 2 wasn't told why attempt 1 failed:\n%s", second)
	}
	if r.State.Usage.InTok != 50 {
		t.Errorf("usage = %+v; the failed session's tokens count", r.State.Usage)
	}
	if len(r.Saved) != 1 || !strings.Contains(r.Saved[0], "discarded") {
		t.Errorf("Saved = %v; what the reset removed is kept in a ref", r.Saved)
	}
}

func TestARetriedSessionStartsFromTheCheckpoint(t *testing.T) {
	script := "steps:\n  a:\n    - actions: [ " + write("half.txt", "half") + " ]\n      error: the connection dropped\n      kind: transient\n" +
		"    - actions: [ " + write("a.txt", "a") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "half.txt")); err == nil {
		t.Error("the retry ran on what the dropped session wrote")
	}
}
