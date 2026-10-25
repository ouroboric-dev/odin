package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/run"
)

// scored makes a finished run whose plan holds the tasks p-T1 and p-T2, and fixes the clock of ygg score.
func scored(t *testing.T) (dir, id, runDir string) {
	t.Helper()
	dir, config := project(t, "script.yaml", "mode: key-points\n")
	id, _ = walk(t, dir, config)
	runDir = run.Dir(dir, id)
	st, err := run.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Features = []schema.FeatureRecord{{Feature: schema.Feature{ID: "p-T1", Layer: "repos/app"}, Status: schema.TaskDone}, {Feature: schema.Feature{ID: "p-T2", Layer: "repos/app"}, Status: schema.TaskTodo}}
	if err := st.Save(runDir); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	was := scoreClock
	scoreClock = func() time.Time { at = at.Add(time.Minute); return at }
	t.Cleanup(func() { scoreClock = was })
	return dir, id, runDir
}

func readJudgements(t *testing.T, runDir string) []schema.Judgement {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, "judgements.json"))
	if err != nil {
		t.Fatal(err)
	}
	var js []schema.Judgement
	if err := json.Unmarshal(data, &js); err != nil {
		t.Fatalf("judgements.json: %v\n%s", err, data)
	}
	return js
}

// judgedLines reads the judged lines of the ledger, after the chain is checked against run.json.
func judgedLines(t *testing.T, runDir string) []gungnir.Event {
	t.Helper()
	st, err := run.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runDir, "ledger.jsonl")
	if _, err := ledger.Verify(path, st.Ledger); err != nil {
		t.Fatalf("the ledger does not verify after a score: %v", err)
	}
	b, _ := os.ReadFile(path)
	var out []gungnir.Event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e gungnir.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Name == gungnir.EventJudged {
			out = append(out, e)
		}
	}
	return out
}

func TestAJudgementReplacesTheLastAndTheLedgerKeepsEach(t *testing.T) {
	dir, id, runDir := scored(t)
	score := func(args ...string) (int, string) {
		code, _, errOut := ygg(t, append([]string{"score", id, "--project", dir}, args...)...)
		return code, errOut
	}
	if code, e := score("--score", "6", "--well", "the plan was tight", "--wrong", "the first loop wandered"); code != ExitDone {
		t.Fatalf("score: exit %d, %s", code, e)
	}
	if code, e := score("--score", "9", "--well", "better on second look", "--wrong", "line one\nline two"); code != ExitDone {
		t.Fatalf("score again: exit %d, %s", code, e)
	}
	if code, e := score("--score", "3", "--task", "p-T1", "--well", "the tests came first", "--wrong", "it ignored the style guide"); code != ExitDone {
		t.Fatalf("score a task: exit %d, %s", code, e)
	}
	js := readJudgements(t, runDir)
	if len(js) != 2 {
		t.Fatalf("judgements.json holds %d judgements, want the run's and the task's: %+v", len(js), js)
	}
	if j := js[0]; j.Run != id || j.Task != "" || j.Score != 9 || j.Well != "better on second look" || j.Wrong != "line one\nline two" || j.Time != "2026-10-25T09:32:00Z" {
		t.Errorf("the run's judgement = %+v; want the second one, at the fixed clock", j)
	}
	if j := js[1]; j.Task != "p-T1" || j.Score != 3 {
		t.Errorf("the task's judgement = %+v", j)
	}
	for _, j := range js {
		b, _ := json.Marshal(j)
		if err := schema.Validate(schema.ContractJudgement, b); err != nil {
			t.Errorf("%+v does not validate: %v", j, err)
		}
	}
	if st, err := os.Stat(filepath.Join(runDir, "judgements.json")); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("judgements.json: %v, %v; want mode 0644", st, err)
	}
	// The ledger keeps each judgement; the score and the task ride in fields the ledger schema has.
	lines := judgedLines(t, runDir)
	if len(lines) != 3 {
		t.Fatalf("%d judged lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Result != "6" || lines[1].Result != "9" || lines[2].Result != "3" || lines[0].Task != "" || lines[2].Task != "p-t1" || !lines[0].Time.Equal(time.Date(2026, 10, 25, 9, 31, 0, 0, time.UTC)) {
		t.Errorf("judged lines = %+v", lines)
	}
	// record.json is rebuilt after each: it holds the judgements as they stand now.
	data, err := os.ReadFile(filepath.Join(runDir, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(schema.ContractRecord, data); err != nil {
		t.Fatalf("record.json: %v", err)
	}
	var rec struct {
		Judgements []schema.Judgement `json:"judgements"`
	}
	if err := json.Unmarshal(data, &rec); err != nil || len(rec.Judgements) != 2 || rec.Judgements[0].Score != 9 || rec.Judgements[1].Task != "p-T1" {
		t.Errorf("record.json judgements = %+v, %v", rec.Judgements, err)
	}
}

func TestAScoreThatTheContractRefusesChangesNothing(t *testing.T) {
	dir, id, runDir := scored(t)
	good := []string{"--well", "a", "--wrong", "b"}
	long := strings.Repeat("x", 16385)
	for name, args := range map[string][]string{
		"above ten":           append([]string{"--score", "11"}, good...),
		"below zero":          append([]string{"--score", "-1"}, good...),
		"no number":           append([]string{"--score", "nine"}, good...),
		"no score":            good,
		"no well":             {"--score", "5", "--wrong", "b"},
		"empty wrong":         {"--score", "5", "--well", "a", "--wrong", ""},
		"a well over bound":   {"--score", "5", "--well", long, "--wrong", "b"},
		"a wrong over bound":  {"--score", "5", "--well", "a", "--wrong", long},
		"a task not planned":  append([]string{"--score", "5", "--task", "p-T9"}, good...),
		"a task as a pattern": append([]string{"--score", "5", "--task", "../p-T1"}, good...),
	} {
		code, _, errOut := ygg(t, append([]string{"score", id, "--project", dir}, args...)...)
		if code != ExitRefused {
			t.Errorf("%s: exit %d, want %d (%s)", name, code, ExitRefused, errOut)
		}
		if name == "above ten" && !strings.Contains(errOut, "maximum") {
			t.Errorf("above ten: %q does not give the contract's error", errOut)
		}
		if name == "a task not planned" && !strings.Contains(errOut, "p-T9") {
			t.Errorf("a task not planned: %q does not name the task", errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "judgements.json")); err == nil {
		t.Error("a refused score wrote judgements.json")
	}
	if n := len(judgedLines(t, runDir)); n != 0 {
		t.Errorf("a refused score logged %d judged lines", n)
	}
	for _, args := range [][]string{{"score", "--project", dir}, {"score", "20261015-090000-a1b2c3", "--project", dir, "--score", "5", "--well", "a", "--wrong", "b"}} {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%v: exit %d, want %d", args, code, ExitRefused)
		}
	}
}

func TestARunInUseIsNotJudgedBehindItsBack(t *testing.T) {
	dir, id, runDir := scored(t)
	unlock, err := run.Lock(runDir) // a ygg process is advancing the run: its ledger is not ours to write
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	code, _, errOut := ygg(t, "score", id, "--project", dir, "--score", "5", "--well", "a", "--wrong", "b")
	if code != ExitRefused || !strings.Contains(errOut, "in use") && !strings.Contains(errOut, "advanc") {
		t.Errorf("exit %d, %q; want a refusal that says the run is in use", code, errOut)
	}
}

func TestAStoppedRunWithNoLockIsJudged(t *testing.T) {
	dir, id, runDir := scored(t)
	st, err := run.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Status = run.StatusRunning // a run killed mid-way still says running, and holds no lock
	if err := st.Save(runDir); err != nil {
		t.Fatal(err)
	}
	if code, _, e := ygg(t, "score", id, "--project", dir, "--score", "0", "--well", "a", "--wrong", "b"); code != ExitDone {
		t.Fatalf("exit %d, %s", code, e)
	}
	if after, _ := run.LoadState(runDir); after.Status != run.StatusRunning {
		t.Errorf("status = %s; a score does not change the run", after.Status)
	}
}

func TestAJudgementIsKeptWhenTheRecordCannotBeRebuilt(t *testing.T) {
	dir, id, runDir := scored(t)
	if err := os.Remove(filepath.Join(runDir, "record.json")); err != nil { // the record the run wrote when it ended
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(runDir, "record.json"), 0o755); err != nil { // the rebuild cannot rename over a folder
		t.Fatal(err)
	}
	code, _, errOut := ygg(t, "score", id, "--project", dir, "--score", "4", "--well", "a", "--wrong", "b")
	if code != ExitFailed {
		t.Errorf("exit %d, want %d: the judgement is kept, the command did not finish", code, ExitFailed)
	}
	for _, want := range []string{"judgement was recorded", "could not be rebuilt", "ygg record " + id} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q does not say %q", errOut, want)
		}
	}
	if js := readJudgements(t, runDir); len(js) != 1 || js[0].Score != 4 {
		t.Errorf("judgements.json = %+v, want the judgement kept", js)
	}
	if n := len(judgedLines(t, runDir)); n != 1 {
		t.Errorf("%d judged lines, want 1", n)
	}
	// The retry the message names works once the cause is gone, and adds no second line.
	if err := os.Remove(filepath.Join(runDir, "record.json")); err != nil {
		t.Fatal(err)
	}
	if code, _, e := ygg(t, "record", id, "--project", dir); code != ExitDone {
		t.Fatalf("record: exit %d, %s", code, e)
	}
	if n := len(judgedLines(t, runDir)); n != 1 {
		t.Errorf("%d judged lines after the retry, want 1", n)
	}
}

// A judgement the ledger took, whose judgements.json then could not be written, is not refused: the ledger holds it,
// and the command says so, and that a second score would log it twice.
func TestAJudgementInTheLedgerOnlyIsAFailureNotARefusal(t *testing.T) {
	dir, id, runDir := scored(t)
	was := writeJudgements
	writeJudgements = func(string, []schema.Judgement) error { return errors.New("disk full") }
	t.Cleanup(func() { writeJudgements = was })
	code, _, errOut := ygg(t, "score", id, "--project", dir, "--score", "4", "--well", "a", "--wrong", "b")
	if code != ExitFailed {
		t.Errorf("exit %d, want %d: the ledger holds the judgement", code, ExitFailed)
	}
	for _, want := range []string{"the judgement is in the ledger", "judgements.json could not be written: disk full", "ygg score again would log it twice"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q does not say %q", errOut, want)
		}
	}
	if n := len(judgedLines(t, runDir)); n != 1 {
		t.Errorf("%d judged lines, want 1", n)
	}
}
