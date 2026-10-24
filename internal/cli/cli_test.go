package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// ygg runs one command in-process and returns its exit code, stdout and stderr.
func ygg(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// project makes a project umbrella in a fresh instance tree, and a factory.yaml for the toy realm
// beside it. Its roles use script, a file in testdata/toy; extra is added to the config as is.
func project(t *testing.T, script, extra string) (dir, config string) {
	t.Helper()
	// Each test has a spend journal of its own, so what one spends never counts against another's day.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	toy, err := filepath.Abs("../../testdata/toy")
	if err != nil {
		t.Fatal(err)
	}
	dir = treetest.Project(t) // the toy realm creates its leaf, repos/app
	config = filepath.Join(t.TempDir(), "factory.yaml")
	s := filepath.Join(toy, script)
	body := extra + "roles:\n  planner: { agent: scripted, model: none, script: " + s + " }\n" +
		"  implementer: { agent: scripted, model: none, script: " + s + " }\n"
	if !strings.Contains(extra, "budget:") {
		body += "budget:\n  per_run: { tokens: 100000, turns: 100 }\n  per_day: { tokens: 200000, turns: 200 }\n"
	}
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, config
}

func toyRealm(t *testing.T) string {
	t.Helper()
	p, _ := filepath.Abs("../../testdata/toy")
	return p
}

// runID returns the id of the only run in dir.
func runID(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".factory", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one run, got %v, %v", entries, err)
	}
	return entries[0].Name()
}

func TestUsageErrorsRefuseAndCreateNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	cases := map[string][]string{
		"no command":            {},
		"an unknown command":    {"deploy"},
		"run without --graph":   {"run", "--project", dir, "--config", config},
		"run with a bad mode":   {"run", "--project", dir, "--config", config, "--graph", toyRealm(t), "--mode", "fast"},
		"approve without gate":  {"approve", "20261015-090000-a1b2c3", "--project", dir},
		"a path as the run id":  {"approve", "../../../tmp", "design-gate", "--project", dir},
		"status of a path":      {"status", "../..", "--project", dir},
		"resume of no such run": {"resume", "20261015-090000-a1b2c3", "--project", dir},
	}
	for name, args := range cases {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, code, ExitRefused)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory")); err == nil {
		t.Error("a refused command created .factory/")
	}
}

func TestRunStopsAndApproveContinues(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point design-gate") {
		t.Fatalf("run: exit %d, %q; want 3 at design-gate", code, out)
	}
	id := runID(t, dir)
	code, out, _ = ygg(t, "status", id, "--project", dir)
	if code != ExitDone || !strings.Contains(out, "waiting:  design-gate (design)") || !strings.Contains(out, "leaf:     repos/app on ygg/"+id+" at ") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	// Each decision takes only its own flag.
	if code, _, _ := ygg(t, "approve", id, "design-gate", "--project", dir, "--reason", "x"); code != ExitRefused {
		t.Errorf("approve --reason: exit %d, want %d", code, ExitRefused)
	}
	if code, _, _ := ygg(t, "reject", id, "design-gate", "--project", dir, "--note", "x"); code != ExitRefused {
		t.Errorf("reject --note: exit %d, want %d", code, ExitRefused)
	}
	// Flags may follow the positional arguments.
	if code, out, errOut := ygg(t, "approve", id, "design-gate", "--project", dir, "--note", "fine"); code != ExitDone {
		t.Errorf("approve: exit %d:\n%s%s", code, out, errOut)
	}
	if code, _, errOut := ygg(t, "resume", id, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "is done") {
		t.Errorf("resume of a done run: exit %d, %q", code, errOut)
	}
}

// A refused command exits 2 and changes nothing, even when it asks for another mode and the config
// changed since the last command (decision 10).
func TestARefusedCommandChangesNothing(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	id := runID(t, dir)
	runDir := filepath.Join(dir, ".factory", "runs", id)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(runDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cfg, _ := os.ReadFile(config)
	os.WriteFile(config, append(cfg, "privacy: strict\n"...), 0o644)
	state, ledger := read("run.json"), read("ledger.jsonl")
	for _, args := range [][]string{
		{"approve", id, "design-gat", "--mode", "unattended"},
		{"reject", id, "design-gate", "--reason", " ", "--mode", "unattended"},
	} {
		if code, _, _ := ygg(t, append(args, "--project", dir)...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", args[0], code, ExitRefused)
		}
		if read("run.json") != state || read("ledger.jsonl") != ledger {
			t.Errorf("a refused %s changed run.json or the ledger", args[0])
		}
	}
	if code, _, _ := ygg(t, "resume", id, "--project", dir, "--mode", "unattended"); code != ExitStopped {
		t.Fatalf("resume of a stopped run: exit %d, want %d", code, ExitStopped)
	}
	if l := read("ledger.jsonl"); !strings.Contains(l, `"event":"config_changed"`) || !strings.Contains(l, `"event":"mode_changed"`) {
		t.Errorf("resume at a pause didn't record the switch:\n%s", l)
	}
	ygg(t, "approve", id, "design-gate", "--project", dir)
	state, ledger = read("run.json"), read("ledger.jsonl")
	if code, _, _ := ygg(t, "resume", id, "--project", dir, "--mode", "step-by-step"); code != ExitRefused {
		t.Errorf("resume of a done run: exit %d, want %d", code, ExitRefused)
	}
	if read("run.json") != state || read("ledger.jsonl") != ledger {
		t.Error("a refused resume of a done run changed run.json or the ledger")
	}
}

// A run that fails after it started is recorded as failed and exits 1, with the resume hint
// (decision 16); a refused start exits 2.
func TestAFailedStartExitsOne(t *testing.T) {
	_, config := project(t, "script.yaml", "")
	dir := treetest.Project(t, "repos/app")
	if _, err := tree.Resolve(context.Background(), dir, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos", "app", "README.md"), []byte("a person's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	id := runID(t, dir)
	if code != ExitFailed || !strings.Contains(errOut, "ygg resume "+id) || !strings.Contains(errOut, "refs/ygg/"+id+"/discarded/") {
		t.Errorf("run with a dirty leaf: exit %d, want %d with the resume hint:\n%s", code, ExitFailed, errOut)
	}
}

// A project made before the graph pin pins a realm at the top of umbrella.yaml (the realm that led its
// runs). It is refused with the way to pin a graph, before anything starts (review focus 1).
func TestARunOnAProjectThatPinsARealmAtTheTopStopsBeforeAnythingStarts(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	f := filepath.Join(dir, "umbrella.yaml")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append([]byte("realm: toy\nrealm_version: 0.3.0\n"), b...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t))
	if code != ExitRefused || !strings.Contains(errOut, "graph and graph_version") {
		t.Errorf("exit %d, stderr %q; want %d and the way to pin a graph", code, errOut, ExitRefused)
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory", "runs")); err == nil {
		t.Error("a refused run left .factory/runs")
	}
}

// --realm is no flag of a run: a leaf's realm is pinned in the project, and --graph names the graph.
func TestRunWithRealmStopsAndSaysUseGraph(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	for name, args := range map[string][]string{
		"realm alone":     {"--realm", "toy"},
		"graph and realm": {"--graph", toyRealm(t), "--realm", "toy"},
	} {
		code, _, errOut := ygg(t, append([]string{"run", "--project", dir, "--config", config}, args...)...)
		if code != ExitRefused || !strings.Contains(errOut, "ygg: use --graph; a leaf's realm is pinned in the project") {
			t.Errorf("%s: exit %d, stderr %q; want %d and the way to name a graph", name, code, errOut, ExitRefused)
		}
		if _, err := os.Stat(filepath.Join(dir, ".factory", "runs")); err == nil {
			t.Errorf("%s: a refused run left .factory/runs", name)
		}
	}
	code, _, errOut := ygg(t, "run", "--project", dir, "--config", config)
	if code != ExitRefused || !strings.Contains(errOut, "run needs --graph") {
		t.Errorf("no flag: exit %d, stderr %q; want %d and run needs --graph", code, errOut, ExitRefused)
	}
}

// A run that a realm led (schema ygg.run/v2) is refused by resume and by status, with the way on.
func TestAResumeOfARunLedByARealmExitsTwoWithTheWayOn(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	id := runID(t, dir)
	file := filepath.Join(dir, ".factory", "runs", id, "run.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(b), `"schema": "ygg.run/v3"`, `"schema": "ygg.run/v2"`, 1)
	if old == string(b) {
		old = strings.Replace(string(b), `"schema":"ygg.run/v3"`, `"schema":"ygg.run/v2"`, 1)
	}
	if err := os.WriteFile(file, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := ygg(t, "resume", id, "--project", dir)
	if code != ExitRefused || !strings.Contains(errOut, "can't be resumed") || !strings.Contains(errOut, "start a new run with --graph") {
		t.Errorf("resume: exit %d, stderr %q", code, errOut)
	}
	code, out, errOut := ygg(t, "status", id, "--project", dir)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "can't be resumed") || !strings.Contains(errOut, "start a new run with --graph") {
		t.Errorf("status: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// writeFile writes body to name in a folder of the test's own and returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stoppedToy starts the toy graph step by step, so it stops at every gate, the first being design-gate, and returns the project, the run's id and its folder.
func stoppedToy(t *testing.T) (dir, id, runDir string) {
	t.Helper()
	dir, config := project(t, "script.yaml", "")
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t), "--mode", "step-by-step")
	if code != ExitStopped {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, `ygg approve `) || !strings.Contains(out, "--decisions <file>") {
		t.Errorf("the stop does not name --decisions:\n%s", out)
	}
	id = runID(t, dir)
	return dir, id, filepath.Join(dir, ".factory", "runs", id)
}

// gateDecisions is what run.json keeps of the gates' decisions.
func gateDecisions(t *testing.T, runDir string) []run.GateDecisions {
	t.Helper()
	s, err := run.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	return s.Decisions
}

func TestApproveTakesADecisionsFile(t *testing.T) {
	dir, id, runDir := stoppedToy(t)
	good := writeFile(t, "decisions.json", `{"decisions": [{"id": "password-reset", "text": "The reset link goes by email.\nNever by SMS."}, `+
		`{"id": "storage-cap", "text": "Each family has 10 GB."}]}`)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	state, _ := os.ReadFile(filepath.Join(runDir, "run.json"))
	ledgerBefore, _ := os.ReadFile(filepath.Join(runDir, "ledger.jsonl"))
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a file and a note":           {[]string{"approve", id, "design-gate", "--decisions", good, "--note", "x"}, "not both"},
		"a file and a reason":         {[]string{"reject", id, "design-gate", "--decisions", good, "--reason", "x"}, "not both"},
		"a reason in an approval":     {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "r.json", `{"reason": "x", "decisions": [{"id": "a", "text": "b"}]}`)}, "reason belongs to a rejection"},
		"a rejection with no reason":  {[]string{"reject", id, "design-gate", "--decisions", good}, "a rejection needs a reason"},
		"a symbolic link":             {[]string{"approve", id, "design-gate", "--decisions", link}, "symbolic link"},
		"a folder":                    {[]string{"approve", id, "design-gate", "--decisions", t.TempDir()}, "not a regular file"},
		"a file over 64 KiB":          {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "big.json", `{"decisions": [{"id": "a", "text": "`+strings.Repeat("x", 70<<10)+`"}]}`)}, "64 KiB"},
		"not the contract":            {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "bad.json", `{"decisions": [{"id": "a", "text": "b", "why": "c"}]}`)}, "ygg.decisions/v1"},
		"an id used twice":            {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "twice.json", `{"decisions": [{"id": "a", "text": "b"}, {"id": "a", "text": "c"}]}`)}, "used twice"},
		"a file that says nothing":    {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "empty.json", `{}`)}, "holds no decision"},
		"a key twice in the document": {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "dup.json", `{"decisions": [{"id": "a", "text": "b"}], "decisions": [{"id": "c", "text": "d"}]}`)}, "key decisions appears twice"},
		"a key twice in a decision":   {[]string{"approve", id, "design-gate", "--decisions", writeFile(t, "dup2.json", `{"decisions": [{"id": "a", "text": "b", "text": "c"}]}`)}, "key text appears twice"},
		"no such file":                {[]string{"approve", id, "design-gate", "--decisions", filepath.Join(t.TempDir(), "none.json")}, "none.json"},
	} {
		code, _, errOut := ygg(t, append(c.args, "--project", dir)...)
		if code != ExitRefused || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, %q; want %d and a refusal that says %q", name, code, errOut, ExitRefused, c.want)
		}
	}
	if s, _ := os.ReadFile(filepath.Join(runDir, "run.json")); string(s) != string(state) {
		t.Error("a refused decision changed run.json")
	}
	if l, _ := os.ReadFile(filepath.Join(runDir, "ledger.jsonl")); string(l) != string(ledgerBefore) {
		t.Error("a refused decision changed the ledger")
	}

	code, out, errOut := ygg(t, "approve", id, "design-gate", "--decisions", good, "--project", dir)
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point spec-gate") {
		t.Fatalf("approve --decisions: exit %d:\n%s%s", code, out, errOut)
	}
	want := []run.GateDecisions{{Gate: "design-gate", Decisions: schema.Decisions{Decisions: []schema.Decision{
		{ID: "password-reset", Text: "The reset link goes by email.\nNever by SMS."}, {ID: "storage-cap", Text: "Each family has 10 GB."}}}}}
	if got := gateDecisions(t, runDir); !reflect.DeepEqual(got, want) {
		t.Errorf("run.json keeps %+v; want %+v", got, want)
	}
	l, _ := os.ReadFile(filepath.Join(runDir, "ledger.jsonl"))
	if !strings.Contains(string(l), `"event":"gate_approved","ygg.gate.name":"design-gate","ygg.note":"{\"decisions\":[{\"id\":\"password-reset\",\"text\":\"The reset link goes by email.\\nNever by SMS.\"}`) {
		t.Errorf("the gate_approved event does not carry the decisions:\n%s", l)
	}

	// A rejection's file holds its reason, which goes back to the node; its decisions are the gate's.
	reject := writeFile(t, "reject.json", `{"reason": "Name the users.", "decisions": [{"id": "two-users", "text": "The owner and a guest."}]}`)
	if code, out, errOut := ygg(t, "reject", id, "spec-gate", "--decisions", reject, "--project", dir); code != ExitStopped {
		t.Fatalf("reject --decisions: exit %d:\n%s%s", code, out, errOut)
	}
	if got := gateDecisions(t, runDir); len(got) != 2 || got[1].Gate != "spec-gate" || got[1].Decisions.Decisions[0].ID != "two-users" {
		t.Errorf("run.json keeps %+v; want the spec gate's decision after the design gate's", got)
	}
}

func TestANoteIsOneDecision(t *testing.T) {
	dir, id, runDir := stoppedToy(t)
	for _, note := range []string{"a\nb", "a\tb", "a\u2028b"} {
		if code, _, errOut := ygg(t, "approve", id, "design-gate", "--note", note, "--project", dir); code != ExitRefused || !strings.Contains(errOut, "one line") {
			t.Errorf("--note %q: exit %d, %q; want a refusal: a note is one line, and a decisions file holds more", note, code, errOut)
		}
	}
	if code, out, errOut := ygg(t, "approve", id, "design-gate", "--note", "Use the warm palette.", "--project", dir); code != ExitStopped {
		t.Fatalf("approve --note: exit %d:\n%s%s", code, out, errOut)
	}
	want := []run.GateDecisions{{Gate: "design-gate", Decisions: schema.Decisions{Decisions: []schema.Decision{{ID: "note-design-gate", Text: "Use the warm palette."}}}}}
	if got := gateDecisions(t, runDir); !reflect.DeepEqual(got, want) {
		t.Errorf("run.json keeps %+v; want the note as one decision with the id note-design-gate", got)
	}
	if code, _, errOut := ygg(t, "reject", id, "spec-gate", "--reason", "a\nb", "--project", dir); code != ExitRefused || !strings.Contains(errOut, "--reason is one line") {
		t.Errorf("--reason over two lines: exit %d, %q; want a refusal", code, errOut)
	}
	// --reason is a file with only the reason.
	if code, out, errOut := ygg(t, "reject", id, "spec-gate", "--reason", "Name the users.", "--project", dir); code != ExitStopped {
		t.Fatalf("reject --reason: exit %d:\n%s%s", code, out, errOut)
	}
	l, _ := os.ReadFile(filepath.Join(runDir, "ledger.jsonl"))
	if !strings.Contains(string(l), `"event":"gate_rejected","ygg.gate.name":"spec-gate","ygg.note":"{\"reason\":\"Name the users.\"}"`) {
		t.Errorf("the gate_rejected event does not carry the reason as a decisions file:\n%s", l)
	}
	if got := gateDecisions(t, runDir); !reflect.DeepEqual(got, want) {
		t.Errorf("run.json keeps %+v after a rejection with only a reason; want the design gate's note alone", got)
	}
}
