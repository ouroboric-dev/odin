package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// loopProject makes a project, a factory.yaml whose implementer and reviewer roles play script, and
// returns the toy loop realm with them. caps is the budget: section.
func loopProject(t *testing.T, script, caps string) (dir, config, realm string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	realm, err := filepath.Abs("../../testdata/toy-loop")
	if err != nil {
		t.Fatal(err)
	}
	dir = treetest.Project(t)
	cfgDir := t.TempDir()
	scriptPath := filepath.Join(cfgDir, "script.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(cfgDir, "factory.yaml")
	if caps == "" {
		caps = "budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	}
	body := "roles:\n  implementer: { agent: scripted, model: none, script: " + scriptPath + " }\n  reviewer: { agent: scripted, model: none, script: " + scriptPath + " }\n" + caps
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, config, realm
}

const implementOnce = `steps:
  build:
    - actions: [ { write: { path: build.txt, content: "done\n" } } ]
      result: { status: ok, summary: built it, usage: { in: 2000, out: 400, turns: 5 }, output: '{"summary": "done"}' }
`

// The exit criterion: a malformed reviewer output stops the loop. Nothing counts it as a pass, an
// approval is refused, and sending the work back asks the reviewer again. Each time it is asked, an answer that is
// not valid is asked for once more, with its errors.
func TestAMalformedReviewerOutputStopsTheLoopForAPerson(t *testing.T) {
	script := implementOnce + `  build-spec:
    - result: { status: ok, output: "LGTM, ship it.", usage: { in: 100, out: 10, turns: 1 } }
    - result: { status: ok, output: "Looks fine to me.", usage: { in: 100, out: 10, turns: 1 } }
    - result: { status: ok, output: "Still fine.", usage: { in: 100, out: 10, turns: 1 } }
    - result: { status: ok, output: "Fine.", usage: { in: 100, out: 10, turns: 1 } }
    - result: { status: ok, report: [], usage: { in: 100, out: 10, turns: 1 } }
  build-quality:
    - result: { status: ok, report: [], usage: { in: 100, out: 10, turns: 1 } }
`
	dir, config, realm := loopProject(t, script, "")
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", realm, "--mode", "unattended")
	id := runID(t, dir)
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point build, which takes no approval") || !strings.Contains(out, "ygg reject "+id+" build --reason") || strings.Contains(out, "ygg approve") {
		t.Fatalf("run: exit %d:\n%s%s\nwant a stop that offers a rejection only", code, out, errOut)
	}
	code, out, _ = ygg(t, "status", id, "--project", dir)
	if code != ExitDone || !strings.Contains(out, "waiting:  build (escalation)") || !strings.Contains(out, "gave up in attempt 1 (reviewer-unreadable)") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	if code, _, errOut := ygg(t, "approve", id, "build", "--project", dir); code != ExitRefused || !strings.Contains(errOut, "can't be read") {
		t.Errorf("approve: exit %d, %q; an answer nobody can read is not an approval", code, errOut)
	}
	if code, out, _ := ygg(t, "resume", id, "--project", dir); code != ExitStopped {
		t.Errorf("resume: exit %d:\n%s; a stopped run stays stopped", code, out)
	}
	code, out, errOut = ygg(t, "reject", id, "build", "--reason", "The reviewer's prompt was fixed.", "--project", dir)
	if code != ExitDone || !strings.Contains(out, "is done") {
		t.Errorf("reject: exit %d:\n%s%s; want the loop to go on to the end", code, out, errOut)
	}
	s, _ := run.LoadState(run.Dir(dir, id))
	if s.Attempts["build"] != 1 || s.Attempts["build-spec"] != 5 {
		t.Errorf("sessions: implementer %d, spec reviewer %d; the implementer must not run again for an unreadable reviewer", s.Attempts["build"], s.Attempts["build-spec"])
	}
}

// The exit criterion: a run that hits a cap stops, with everything resumable.
func TestARunThatHitsACapStopsResumableAndGoesOnWhenTheCapIsRaised(t *testing.T) {
	script := `steps:
  build:
    - actions: [ { write: { path: build.txt, content: "todo\n" } } ]
      result: { status: ok, usage: { in: 2000, out: 400, turns: 5 }, output: '{"summary": "done"}' }
    - actions: [ { write: { path: build.txt, content: "done\n" } } ]
      result: { status: ok, usage: { in: 2000, out: 400, turns: 5 }, output: '{"summary": "done"}' }
  build-spec:    [ { result: { status: ok, report: [] } } ]
  build-quality: [ { result: { status: ok, report: [] } } ]
`
	caps := "budget:\n  per_run: { tokens: 2000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	dir, config, realm := loopProject(t, script, caps)
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", realm)
	id := runID(t, dir)
	if code != ExitStopped || !strings.Contains(out, "stopped at the hard approval point budget-cap") || strings.Contains(out, "ygg reject") {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	code, out, _ = ygg(t, "status", id, "--project", dir)
	if code != ExitDone || !strings.Contains(out, "waiting:  build (budget-cap)") || !strings.Contains(out, "loop:     build: attempt 2 of 3, implement, 1 finding(s) open") ||
		!strings.Contains(out, "caps:     this run 2400 of 2000 tokens") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	// Approving while the cap is reached stops again at once; raising the cap lets the run go on.
	if code, out, _ := ygg(t, "approve", id, "build", "--project", dir); code != ExitStopped || !strings.Contains(out, "budget-cap") {
		t.Errorf("approve with the cap still reached: exit %d:\n%s", code, out)
	}
	b, _ := os.ReadFile(config)
	os.WriteFile(config, []byte(strings.Replace(string(b), "tokens: 2000", "tokens: 30000", 1)), 0o644)
	code, out, errOut = ygg(t, "approve", id, "build", "--project", dir)
	if code != ExitDone {
		t.Errorf("approve after the cap was raised: exit %d:\n%s%s", code, out, errOut)
	}
	ledger, _ := os.ReadFile(filepath.Join(dir, ".factory", "runs", id, "ledger.jsonl"))
	if !strings.Contains(string(ledger), `"event":"config_changed"`) || !strings.Contains(string(ledger), `"event":"run_finished"`) {
		t.Errorf("the ledger:\n%s", ledger)
	}
}

// The exit criterion: a finding closes only with proof from its source. The implementer saying it fixed
// the problem changes nothing; the reviewer closes it, with a file and line and a check that passed.
func TestAFindingClosesOnlyWhenItsSourceConfirmsItWithProof(t *testing.T) {
	major := `{ id: spec-1, source: spec, severity: major, location: "build.txt:1", claim: "build.txt doesn't say done twice", status: open }`
	script := `steps:
  build:
    - actions: [ { write: { path: build.txt, content: "done\n" } } ]
      result: { status: ok, summary: built it, output: '{"summary": "done"}' }
    - actions: [ { write: { path: build.txt, content: "done done\n" } } ]
      result: { status: ok, summary: "I fixed spec-1; it is closed.", output: '{"summary": "done"}' }
    - actions: [ { write: { path: build.txt, content: "done done\n" } } ]
      result: { status: ok, summary: "spec-1 is fixed, really.", output: '{"summary": "done"}' }
  build-spec:
    - result: { status: ok, report: [ ` + major + ` ] }
    - result: { status: ok, report: [ ` + major + ` ] }
    - result: { status: ok, report: [ { id: spec-1, source: spec, severity: major, location: "build.txt:1", claim: "now fixed", status: fixed, proof: "build.txt:1 says done twice; check unit passed" } ] }
  build-quality: [ { result: { status: ok, report: [] } } ]
`
	dir, config, realm := loopProject(t, script, "")
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", realm)
	if code != ExitDone {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	id := runID(t, dir)
	ledger, _ := os.ReadFile(filepath.Join(dir, ".factory", "runs", id, "ledger.jsonl"))
	text := string(ledger)
	if strings.Count(text, `"event":"finding_raised"`) != 1 || strings.Count(text, `"event":"finding_closed"`) != 1 || !strings.Contains(text, `"ygg.finding.attempts":3`) {
		t.Errorf("want one finding, raised once and closed once after 3 attempts:\n%s", text)
	}
	if strings.Index(text, "I fixed spec-1") > 0 && strings.Index(text, `"event":"finding_closed"`) < strings.Index(text, "I fixed spec-1") {
		t.Error("the finding closed before the reviewer confirmed it")
	}
	s, _ := run.LoadState(run.Dir(dir, id))
	if f := s.Loops["build"].State.Book.Records["spec-1"]; f == nil || f.Status != "fixed" || f.Closed != 3 || !strings.Contains(f.Proof, "check unit passed") {
		t.Errorf("record = %+v", f)
	}
}
