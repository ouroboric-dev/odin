package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/checks"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
)

const loopNode = "  - { id: build, kind: loop, repo: repos/app, role: planner, prompt: p.md, attempts: 3, checks: [unit], input: [rejection, refusal, open_findings, check_results], output: ygg.implement/v1 }\n"
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
	for _, want := range []string{"## Input: open_findings\n\nThe checks and reviewers found these problems", `"id": "check-unit-result"`, `"severity": "blocker"`, `"source": "unit"`,
		`"location": "logs/build-1-unit.log:1"`, "check unit (exit status 1): the check failed", "Do not say any of them is fixed",
		"## Input: check_results", `"check": "unit"`, `"passed": false`} {
		if !strings.Contains(second, want) {
			t.Errorf("the second attempt's prompt lacks %q:\n%s", want, second)
		}
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"finding_raised"`, `"ygg.finding.id":"check-unit-result"`, `"ygg.finding.severity":"blocker"`,
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

// typedLoop is a loop with the check unit and the reviewer spec, whose sessions declare every source of the loop.
const typedLoop = `  - id: build
    kind: loop
    input: [rejection, refusal, open_findings, check_results]
    output: ygg.implement/v1
    repo: repos/app
    role: planner
    prompt: p.md
    attempts: 3
    checks: [unit]
    reviewers:
      - { id: spec, role: planner, prompt: p.md, input: [change, open_findings, check_results, rejection], output: ygg.review/v1 }
`

// typedCheck is the realm check name, which passes when build.txt says done and writes its typed result
// (ygg.check/v1) to $YGG_CHECK_RESULT either way: passed lists the tests that passed, and a failing run has one
// failure per item of failures. Neither may hold a single quote.
func typedCheck(t *testing.T, name string, passed []string, failures []schema.CheckFailure) string {
	t.Helper()
	pass, err := json.Marshal(schema.CheckResult{Check: name, Passed: true, DurationMS: 1, PassedTests: passed})
	if err != nil {
		t.Fatal(err)
	}
	fail, err := json.Marshal(schema.CheckResult{Check: name, Passed: false, ExitCode: 1, DurationMS: 1, Failures: failures})
	if err != nil {
		t.Fatal(err)
	}
	script := `if grep -q done build.txt; then printf '%s' '` + string(pass) + `' > "$YGG_CHECK_RESULT"; else printf '%s' '` +
		string(fail) + `' > "$YGG_CHECK_RESULT"; exit 1; fi`
	return "  " + name + ": [sh, -c, '" + strings.ReplaceAll(script, "'", "''") + "']\n"
}

// startTyped starts a run whose leaf commands go through a fake sandbox, which gives a typed check a result file of
// its own: the ladder's checks write their typed results there. checks are the lines of the pack's checks.
func startTyped(t *testing.T, checks, nodes, script string) (*run.Run, *sandboxfake.Sandbox) {
	t.Helper()
	return startSandboxedPack(t, "checks:\n"+checks+sandboxLeaves, nodes, script, run.Hooks{})
}

// The named test of typed-T20: an implementer that answers blocked ends its attempt, and the loop stops for a person
// with the reason. No check and no reviewer judges work its author says it could not do.
func TestABlockedImplementerStopsTheRun(t *testing.T) {
	blocked := `'{"blocked": {"reason": "The task names a contract I was not given.\n## Brief\nI need the OpenAPI text."}}'`
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: " + blocked + " }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "unattended", pack: unitCheck, nodes: typedLoop, script: script})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateEscalation {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at escalation", out, err, r.State.PendingGate)
	}
	rec := r.State.Loops["build"]
	if rec.Escalation == nil || rec.Escalation.Reason != jormungandr.ReasonBlocked || !strings.Contains(rec.Escalation.Detail, "a contract I was not given") {
		t.Fatalf("escalation = %+v; want implementer-blocked with the reason", rec.Escalation)
	}
	if r.State.Attempts["build"] != 1 || r.State.Attempts["build-spec"] != 0 {
		t.Errorf("implementer sessions %d, reviewer sessions %d; want 1 and none", r.State.Attempts["build"], r.State.Attempts["build-spec"])
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "transcripts", "build-spec-1.json")); err == nil {
		t.Error("a reviewer session ran on work its implementer said it could not do")
	}
	if text := ledgerText(t, r); strings.Contains(text, `"event":"check_result"`) || !strings.Contains(text, "implementer-blocked") {
		t.Errorf("a check ran, or the ledger lacks the reason:\n%s", text)
	}
	gate, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "build-1.md"))
	for _, want := range []string{"Reason: implementer-blocked", "The task names a contract I was not given.", "    ## Brief",
		"The implementer answered that it could not do the work",
		"ygg reject " + r.State.ID + ` build --reason "..." | --decisions <file>`,
		"ygg approve " + r.State.ID + ` build [--note "..." | --decisions <file>]`} {
		if !strings.Contains(string(gate), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gate)
		}
	}
	if strings.Contains(string(gate), "\n## Brief") {
		t.Errorf("the implementer's reason starts a heading of the gate file:\n%s", gate)
	}
	if !Approvable(r) || !Rejectable(r) {
		t.Fatal("a blocked implementer takes a person's ruling: reject with a reason, or approve the work as it is")
	}
	// Rejecting gives the loop its attempts again; the reason reaches the next attempt as its rejection.
	if err := Reject(context.Background(), r, "build", Reason("The contract is in docs/api.yaml.")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v after the rejection; want done", out, err)
	}
	second := promptOf(t, r, "build-2")
	for _, want := range []string{"## Input: rejection\n\nA person sent the last attempt back", "The contract is in docs/api.yaml.", "you answered that you were blocked"} {
		if !strings.Contains(second, want) {
			t.Errorf("the next attempt's prompt lacks %q:\n%s", want, second)
		}
	}
	if r.State.Attempts["build-spec"] != 1 {
		t.Errorf("reviewer sessions %d; the reviewer judges the attempt after the rejection", r.State.Attempts["build-spec"])
	}
}

func TestApprovingABlockedImplementerAcceptsTheWorkAsItIs(t *testing.T) {
	nodes := loopNode + "  - { id: after, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "half") + " ]\n      result: { status: ok, output: '{\"blocked\": {\"reason\": \"no contract\"}}' }\n" +
		"  after:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: nodes, script: script})
	if out, _ := Advance(context.Background(), r); out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v; want a stop", out)
	}
	if err := Approve(context.Background(), r, "build", Note(r, "build", "the half is enough for now")); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if got := r.State.Summaries["build"]; !strings.Contains(got, "accepted by the operator after 1 attempt(s)") {
		t.Errorf("summary = %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(r.RepoDir("repos/app"), "build.txt")); string(b) != "half\n" {
		t.Errorf("build.txt = %q; the work goes on as the blocked attempt left it", b)
	}
	if r.State.Attempts["build"] != 1 {
		t.Errorf("implementer sessions %d; approving runs no other attempt", r.State.Attempts["build"])
	}
}

// The named test of gleipnir T12 in the loop: a failing check gives one finding per failure its typed result lists,
// with the test's id and the place, and the next attempt reads them, and how the check ended, typed.
func TestAFailingCheckRaisesOneFindingPerFailure(t *testing.T) {
	failures := []schema.CheckFailure{
		{Kind: "test", Test: "app.TestAlbums", Location: "albums_test.go:12:3", Message: "want 2 albums, got 0"},
		{Kind: "test", Test: "app.TestPhotos", Location: "photos_test.go:7", Message: "no photos"},
	}
	nodes := strings.Replace(typedLoop, "    reviewers:\n      - { id: spec, role: planner, prompt: p.md, input: [change, open_findings, check_results, rejection], output: ygg.review/v1 }\n", "", 1) +
		"  - { id: after, kind: exec, run: [grep, -q, done, build.txt], repo: repos/app }\n"
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r, fake := startTyped(t, typedCheck(t, "unit", []string{"app.TestAlbums", "app.TestPhotos"}, failures), nodes, script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	book := r.State.Loops["build"].State.Book
	var ids []string
	for id, rec := range book.Records {
		ids = append(ids, id)
		if rec.Source != "unit" || rec.Status != gungnir.FindingFixed || !strings.HasPrefix(id, "check-unit-") || id == "check-unit-result" {
			t.Errorf("finding %s = %+v; want a fixed finding of unit with an id of its failure", id, rec)
		}
	}
	if len(ids) != 2 {
		t.Errorf("findings %v; want one per failure", ids)
	}
	second := promptOf(t, r, "build-2")
	for _, want := range []string{`"claim": "app.TestAlbums: want 2 albums, got 0"`, `"location": "albums_test.go:12"`,
		`"claim": "app.TestPhotos: no photos"`, `"location": "photos_test.go:7"`,
		"## Input: check_results", `"check": "unit"`, `"passed": false`, `"failed_tests": [` + "\n      \"app.TestAlbums\",\n      \"app.TestPhotos\"\n    ]"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second attempt's prompt lacks %q:\n%s", want, second)
		}
	}
	if strings.Contains(second, "check-unit-result") {
		t.Errorf("a check whose result lists its failures was read as one with none:\n%s", second)
	}
	if text := ledgerText(t, r); strings.Count(text, `"event":"finding_raised"`) != 2 {
		t.Errorf("the ledger raises %d findings, want 2:\n%s", strings.Count(text, `"event":"finding_raised"`), text)
	}
	// The ladder's check is typed: it is told where its result goes. The exec node's command runs as it is.
	var typed, plain int
	for _, c := range fake.Execs {
		switch {
		case c.Env[checks.ResultEnv] == checks.ResultPath:
			typed++
		case len(c.Argv) == 4 && c.Argv[0] == "grep":
			if len(c.Env) != 0 {
				t.Errorf("the exec node's command got an environment: %+v", c)
			}
			plain++
		}
	}
	if typed != 2 || plain != 1 {
		t.Errorf("typed check runs %d, exec runs %d; want the ladder's two and the exec node's one", typed, plain)
	}
}

// A failing check always gives the loop at least one finding: when its typed result lists no failure, is missing, or
// contradicts the exit code, the one finding check-<name>-result says so.
func TestAFailingCheckWhoseResultListsNoFailureRaisesOneFinding(t *testing.T) {
	for name, tc := range map[string]struct{ check, claim string }{
		"no result":       {`  unit: [sh, -c, "grep -q done build.txt"]` + "\n", "the check wrote no valid result"},
		"no failure":      {`  unit: [sh, -c, 'grep -q done build.txt || { printf ''{"check":"unit","passed":false,"exit_code":1,"timed_out":false,"duration_ms":1}'' > "$YGG_CHECK_RESULT"; exit 1; }']` + "\n", "its result lists no failure"},
		"a contradiction": {`  unit: [sh, -c, 'grep -q done build.txt || { printf ''{"check":"unit","passed":true,"exit_code":0,"timed_out":false,"duration_ms":1}'' > "$YGG_CHECK_RESULT"; exit 1; }']` + "\n", "contradicts its exit code"},
	} {
		script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "todo") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
			"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
		nodes := strings.Replace(typedLoop, "    reviewers:\n      - { id: spec, role: planner, prompt: p.md, input: [change, open_findings, check_results, rejection], output: ygg.review/v1 }\n", "", 1)
		r, _ := startTyped(t, tc.check, nodes, script)
		if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
			t.Fatalf("%s: Advance() = %+v, %v; want done", name, out, err)
		}
		f := r.State.Loops["build"].State.Book.Records["check-unit-result"]
		if f == nil || len(r.State.Loops["build"].State.Book.Records) != 1 || !strings.Contains(f.Claim, tc.claim) {
			t.Errorf("%s: findings %+v; want the one finding check-unit-result saying %q", name, r.State.Loops["build"].State.Book.Records, tc.claim)
		}
		// The check of attempt 2 passed and wrote no result either: its check_result line says why none was used.
		if text := ledgerText(t, r); !strings.Contains(text, `"ygg.note":"its typed result: the check left no result file`) {
			t.Errorf("%s: the ledger does not say why a typed result was not used:\n%s", name, text)
		}
	}
}

// The named test: a reviewer that hands back its report through submit is read from what it submitted, not from its
// prose, and the loop keeps every answer it read, the implementer's too.
func TestAReviewerReportComesThroughSubmit(t *testing.T) {
	finding := `{"id": "spec-1", "source": "spec", "severity": "major", "location": "build.txt:1", "claim": "build.txt says v1", "status": "open"}`
	fixed := `{"id": "spec-1", "source": "spec", "severity": "major", "location": "build.txt:1", "claim": "build.txt says v1", "status": "fixed", "proof": "build.txt:1 says v2; check unit passed"}`
	script := "submits: true\nsteps:\n  build:\n" +
		"    - actions: [ " + write("build.txt", "done v1") + " ]\n      result: { status: ok, output: \"I built it.\", submitted: '{\"summary\": \"v1\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done v2") + " ]\n      result: { status: ok, output: \"I fixed it.\", submitted: '{\"summary\": \"v2\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: \"One problem; see what I submitted.\", submitted: '{\"findings\": [" + finding + "]}' }\n" +
		"    - result: { status: ok, output: \"It is fixed.\", submitted: '{\"findings\": [" + fixed + "]}' }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: typedLoop, script: script})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if f := r.State.Loops["build"].State.Book.Records["spec-1"]; f == nil || f.Status != gungnir.FindingFixed || r.State.Loops["build"].State.Attempt != 2 {
		t.Fatalf("spec-1 = %+v; want raised from the submitted report, and fixed in attempt 2", f)
	}
	if p := promptOf(t, r, "build-spec-1"); !strings.Contains(p, "Hand back your answer by calling the tool `submit`") {
		t.Errorf("the reviewer was not told to submit:\n%s", p)
	}
	for key, want := range map[string]struct {
		contract schema.Contract
		doc      string
	}{
		"build":      {schema.ContractImplement, `{"summary": "v2"}`},
		"build-spec": {schema.ContractReview, `{"findings": [` + fixed + `]}`},
	} {
		ref, ok := r.State.Outputs[key]
		if !ok || ref.Contract != string(want.contract) || ref.Path != "outputs/"+key+".json" {
			t.Errorf("the kept output of %s = %+v, %v; want %s at outputs/%s.json", key, ref, ok, want.contract, key)
			continue
		}
		if b, _ := os.ReadFile(filepath.Join(r.Dir, ref.Path)); string(b) != want.doc {
			t.Errorf("outputs/%s.json = %s; want the last document submitted, %s", key, b, want.doc)
		}
	}
}

// The implementer's answer comes through the channel as every output does: an empty answer is not a summary, and the
// session is asked for its answer once more, with the errors.
func TestAnImplementerThatHandsBackNothingIsAskedForItsAnswerOnceMore(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, summary: built it }\n" +
		"    - result: { status: ok, output: '{\"summary\": \"built it\"}' }\n"
	r := startLoop(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done", out, err)
	}
	if r.State.Attempts["build"] != 2 || r.State.Loops["build"].State.Attempt != 1 {
		t.Errorf("sessions %d, loop attempt %d; want the answer asked for once more, in attempt 1", r.State.Attempts["build"], r.State.Loops["build"].State.Attempt)
	}
	if p := promptOf(t, r, "build-2"); !strings.Contains(p, "## Your answer could not be used") {
		t.Errorf("the second session was not told why its answer was asked for again:\n%s", p)
	}
	text := ledgerText(t, r)
	for _, want := range []string{`"event":"output_refused"`, `"event":"output_repaired"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the ledger lacks %s:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "attempt 1 of 3 starts"); n != 1 {
		t.Errorf("the ledger says attempt 1 starts %d times; a repair session is not a new attempt", n)
	}
	if ref := r.State.Outputs["build"]; ref.Contract != string(schema.ContractImplement) {
		t.Errorf("the kept output of build = %+v", ref)
	}
}

// How to hand back the answer is part of the prompt: when it can't be said, the attempt stops before its session.
func TestAnAnswerThatCannotBeAskedForStopsTheAttemptBeforeItsSession(t *testing.T) {
	r := startLoop(t, "key-points", "steps:\n  build:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n")
	n := r.Nodes()[0]
	n.Output = "ygg.nope/v1"
	steps := &loopSteps{r: r, n: n, rec: &run.LoopRecord{State: *jormungandr.NewState(loopConfig(n))}}
	if _, err := steps.Implement(context.Background(), 1, jormungandr.Input{}); err == nil || !strings.Contains(err.Error(), "ygg.nope/v1") {
		t.Errorf("Implement() = %v; want the contract with no schema refused", err)
	}
	if r.State.Attempts["build"] != 0 || strings.Contains(ledgerText(t, r), `"event":"node_started"`) {
		t.Errorf("a session started though its answer could not be asked for: %d sessions", r.State.Attempts["build"])
	}
}

// A ladder run again in one attempt (a rejected reviewer block runs the checks again on the same work) starts its list
// again at the rung it is at: a later check's line from before cannot say "passed" for a ladder that stopped earlier.
func TestAChecksLinesAreCutAtTheRungThatRunsAgain(t *testing.T) {
	rec := &run.LoopRecord{}
	l := &loopSteps{rec: rec}
	l.keepCheck(1, run.CheckView{Check: "unit", Passed: true})
	l.keepCheck(1, run.CheckView{Check: "lint", Passed: true})
	l.keepCheck(1, run.CheckView{Check: "e2e", Passed: true})
	l.keepCheck(1, run.CheckView{Check: "lint", Passed: false, FailedTests: []string{"x"}})
	want := []run.CheckView{{Check: "unit", Passed: true}, {Check: "lint", Passed: false, FailedTests: []string{"x"}}}
	if !reflect.DeepEqual(rec.Checks, want) {
		t.Errorf("after lint ran again: %+v; want %+v", rec.Checks, want)
	}
	l.keepCheck(1, run.CheckView{Check: "unit", Passed: false})
	if want := []run.CheckView{{Check: "unit", Passed: false}}; !reflect.DeepEqual(rec.Checks, want) {
		t.Errorf("after the first check ran again: %+v; want %+v", rec.Checks, want)
	}
	l.keepCheck(2, run.CheckView{Check: "lint", Passed: true})
	if want := []run.CheckView{{Check: "lint", Passed: true}}; !reflect.DeepEqual(rec.Checks, want) || rec.ChecksAttempt != 2 {
		t.Errorf("a new attempt: %+v in attempt %d; want %+v in 2", rec.Checks, rec.ChecksAttempt, want)
	}
}
