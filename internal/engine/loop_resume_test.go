package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

func raiseCap(t *testing.T, r *run.Run, from, to string) *run.Run {
	t.Helper()
	cfg, err := os.ReadFile(r.Setup.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), from) {
		t.Fatalf("factory.yaml lacks %q:\n%s", from, cfg)
	}
	if err := os.WriteFile(r.Setup.ConfigPath, []byte(strings.Replace(string(cfg), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	return reopen(t, r)
}

func TestACapBetweenTwoAttemptsStopsTheRunAndTheSameAttemptGoesOnWhenItIsRaised(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{mode: "unattended", pack: unitCheck, nodes: loopNode, script: script, budget: caps(110, 100, 200000, 200)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at budget-cap in attempt 2", out, err, r.State.PendingGate)
	}
	st := r.State.Loops["build"].State
	if st.Attempt != 2 || st.Phase != "implement" || len(st.Book.Open()) != 1 || r.State.Attempts["build"] != 1 {
		t.Errorf("loop state %+v, %d sessions; want attempt 2 not yet started, with the check's finding open", st, r.State.Attempts["build"])
	}
	r = raiseCap(t, r, "tokens: 110", "tokens: 1000")
	if err := Approve(context.Background(), r, "build", "raised it"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := r.State.Loops["build"].State.Attempt; got != 2 || r.State.Attempts["build"] != 2 {
		t.Errorf("the loop ended in attempt %d with %d sessions; a stop for a cap must not use up an attempt", got, r.State.Attempts["build"])
	}
	if !strings.Contains(transcript(t, r, "build-2"), "- check-unit (blocker") {
		t.Error("the finding was lost across the stop")
	}
}

func TestACapBeforeAReviewerStopsAtTheLadderAndAResumeDoesNotRunTheImplementerAgain(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: reviewLoop, script: script, budget: caps(110, 100, 200000, 200)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateBudgetCap || r.State.Attempts["build-spec"] != 0 {
		t.Fatalf("Advance() = %+v, %v, gate %s, reviewer sessions %d; want a stop before the reviewer", out, err, r.State.PendingGate, r.State.Attempts["build-spec"])
	}
	if got := r.State.Loops["build"].State.Phase; got != "ladder" {
		t.Errorf("the loop is in phase %s, want ladder", got)
	}
	r = raiseCap(t, r, "tokens: 110", "tokens: 1000")
	Approve(context.Background(), r, "build", "")
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone || r.State.Attempts["build"] != 1 || r.State.Attempts["build-spec"] != 1 {
		t.Errorf("Advance() = %+v, %v; implementer sessions %d, reviewer sessions %d; want done with 1 and 1", out, err, r.State.Attempts["build"], r.State.Attempts["build-spec"])
	}
}

func TestAQuotaUsedUpInTheMiddleOfTheLadderPausesTheRunAndTheLadderRunsAgain(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - { error: \"usage limit reached\", kind: quota, usage: { in: 30, out: 3, turns: 1 } }\n    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusPaused {
		t.Fatalf("Advance() = %+v, %v; want a paused run", out, err)
	}
	r = reopen(t, r)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone || r.State.Attempts["build"] != 1 || r.State.Attempts["build-spec"] != 2 {
		t.Errorf("Advance() = %+v, %v; implementer sessions %d, reviewer sessions %d; want done with 1 and 2", out, err, r.State.Attempts["build"], r.State.Attempts["build-spec"])
	}
}

func TestATransientFailureOfAReviewerIsRetriedWithoutUsingAnAttempt(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - { error: \"HTTP 529 overloaded\", kind: transient }\n    - result: { status: ok, report: [] }\n"
	w := &waits{}
	r := startWith(t, setup{pack: unitCheck, nodes: reviewLoop, script: script, hooks: hooks(w)})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone || len(w.got) != 1 || r.State.Loops["build"].State.Attempt != 1 {
		t.Errorf("Advance() = %+v, %v; %d waits; want done in attempt 1 after one retry", out, err, len(w.got))
	}
}

func TestARunInterruptedInAnAttemptResumesThatAttemptWithTheFindings(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("half.txt", "half a thought") + " ]\n      hang: true\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startLoop(t, "key-points", script)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := Advance(ctx, r)
	if err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want a failed run after the interrupt", out, err)
	}
	r = reopen(t, r)
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "half.txt")); err == nil {
		t.Error("the interrupted attempt's half work is still in the leaf after the resume")
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("the resume: %+v, %v", out, err)
	}
	if got := r.State.Loops["build"].State.Attempt; got != 2 {
		t.Errorf("the loop ended in attempt %d; the interrupted attempt 2 must be done again, not counted twice", got)
	}
	if !strings.Contains(transcript(t, r, "build-3"), "- check-unit (blocker") {
		t.Error("the resumed attempt wasn't given the finding")
	}
}

func TestACheckThatPlantsARepositoryInTheLeafStopsTheRunAtPolicyViolationAndTheLoopDoesNotGoOn(t *testing.T) {
	pack := "checks:\n  unit: [sh, -c, \"mkdir -p vendor/x/.git && grep -q done build.txt\"]\n"
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{mode: "unattended", pack: pack, nodes: loopNode, script: script})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GatePolicyViolation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a hard stop at policy-violation", out, err, r.State.PendingGate)
	}
	if r.State.Attempts["build"] != 1 {
		t.Errorf("the loop ran %d sessions; a policy violation is not an attempt to retry", r.State.Attempts["build"])
	}
	// A resume keeps the quarantine: the leaf stays as it was left, for investigation.
	r = reopen(t, r)
	if _, err := os.Stat(filepath.Join(r.RepoDir("repos/app"), "vendor/x/.git")); err != nil {
		t.Error("a resume cleaned a quarantined leaf")
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusStopped || r.State.Attempts["build"] != 1 {
		t.Errorf("Advance() = %+v, %v; a quarantined run is never resumed on its own", out, err)
	}
}

// Review I5: a stop for a cap before the implementer's session doesn't log the attempt as started.
func TestACapBetweenTwoAttemptsLogsTheAttemptStartOnce(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{mode: "unattended", pack: unitCheck, nodes: loopNode, script: script, budget: caps(110, 100, 200000, 200)})
	if out, err := Advance(context.Background(), r); err != nil || r.State.PendingGate != gungnir.GateBudgetCap {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at budget-cap in attempt 2", out, err, r.State.PendingGate)
	}
	if n := strings.Count(ledgerText(t, r), "attempt 2 of 3 starts"); n != 0 {
		t.Errorf("the ledger says attempt 2 started %d time(s) before its session could start", n)
	}
	r = raiseCap(t, r, "tokens: 110", "tokens: 1000")
	if err := Approve(context.Background(), r, "build", "raised it"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if n := strings.Count(ledgerText(t, r), "attempt 2 of 3 starts"); n != 1 {
		t.Errorf("the ledger says attempt 2 started %d time(s); want once", n)
	}
}
