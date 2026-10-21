package score

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

// thisAttempt is the key the tests give ParseStdout; Odin makes 32 hex characters for each attempt.
var thisAttempt = strings.Repeat("0123456789abcdef", 2)

// line is what a suite prints: the marker, the key and the report on one line.
func line(t *testing.T, k, report string) string {
	t.Helper()
	var one bytes.Buffer
	if err := json.Compact(&one, []byte(report)); err != nil {
		one.WriteString(report) // a report that is not JSON stays as it is, for a test of the parser's refusal
	}
	return "ygg-results/v1 " + k + " " + one.String() + "\n"
}

// parse is ParseStdout of the report on a keyed line, with noise before and after it.
func parse(t *testing.T, report string) (map[string]gungnir.ScenarioStatus, error) {
	t.Helper()
	return ParseStdout([]byte("starting the app\n\n"+line(t, thisAttempt, report)+"done\n"), thisAttempt)
}

// A report in Playwright's JSON reporter format: nested suites, a retried test, a project per browser.
const playwrightReport = `{"config":{"rootDir":"/grader"},"suites":[
 {"title":"notes.spec.ts","file":"notes.spec.ts","specs":[
   {"title":"a thing works","ok":true,"tests":[{"projectName":"chromium","results":[{"status":"failed"},{"status":"passed"}]}]},
   {"title":"a thing stays","ok":true,"tests":[{"results":[{"status":"passed"}]}]}],
  "suites":[{"title":"a group","specs":[
   {"title":"a second thing works","tests":[{"results":[{"status":"passed"}]},{"results":[{"status":"timedOut"}]}]},
   {"title":"a thing is private","tests":[{"results":[{"status":"skipped"}]}]}]}]}],
 "errors":[]}`

func TestParseStdoutReadsNestedSuitesAndTheLastTryOfEachTest(t *testing.T) {
	got, err := parse(t, playwrightReport)
	if err != nil {
		t.Fatalf("ParseStdout() = %v", err)
	}
	want := map[string]gungnir.ScenarioStatus{
		"a thing works":        gungnir.ScenarioPassed, // it failed once and the retry passed
		"a thing stays":        gungnir.ScenarioPassed,
		"a second thing works": gungnir.ScenarioFailed, // one of its two projects timed out
		"a thing is private":   gungnir.ScenarioSkipped,
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%q = %q, want %q", id, got[id], st)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d specs, want %d", len(got), len(want))
	}
}

func TestParseStdoutRefusesWhatItCannotReadExactly(t *testing.T) {
	var m *MismatchError
	_, err := parse(t, `{"suites":[{"specs":[{"title":"x","tests":[]},{"title":"x","tests":[]}]}]}`)
	if !errors.As(err, &m) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("a spec title used twice: %v", err)
	}
	for name, body := range map[string]string{"not json": `{"suites":`, "an empty report": ``, "no suites": `{"suites":[]}`} {
		if _, err := parse(t, body); err == nil {
			t.Errorf("%s: ParseStdout() = nil", name)
		}
	}
}

func TestAFailedLastTryIsAFailureWhateverCameBefore(t *testing.T) {
	body := `{"suites":[{"specs":[
	 {"title":"passes then fails","tests":[{"results":[{"status":"passed"},{"status":"failed"}]}]},
	 {"title":"interrupted","tests":[{"results":[{"status":"interrupted"}]}]},
	 {"title":"a status nobody knows","tests":[{"results":[{"status":"flaky-ish"}]}]},
	 {"title":"ran in one project, skipped in another","tests":[{"results":[{"status":"passed"}]},{"results":[{"status":"skipped"}]}]},
	 {"title":"no tests","tests":[]}]}]}`
	got, err := parse(t, body)
	if err != nil {
		t.Fatal(err)
	}
	for title, want := range map[string]gungnir.ScenarioStatus{
		"passes then fails": gungnir.ScenarioFailed, "interrupted": gungnir.ScenarioFailed, "a status nobody knows": gungnir.ScenarioFailed,
		"ran in one project, skipped in another": gungnir.ScenarioSkipped, "no tests": gungnir.ScenarioSkipped,
	} {
		if got[title] != want {
			t.Errorf("%q = %q, want %q", title, got[title], want)
		}
	}
}

func TestAKeyedLineAmongOtherOutputParses(t *testing.T) {
	report := line(t, thisAttempt, playwrightReport)
	for name, out := range map[string]string{
		"only the line":                                 report,
		"in the middle":                                 "noise\n" + report + "more noise\n",
		"ending the output, no newline":                 "noise\n" + strings.TrimSuffix(report, "\n"),
		"after a line of another key":                   line(t, "ffffffffffffffffffffffffffffffff", playwrightReport) + "noise\n" + report,
		"after a line that only starts with the marker": "ygg-results/v1\nygg-results/v1x " + thisAttempt + " {}\n" + report,
		"with CRLF line ends":                           "noise\r\n" + strings.TrimSuffix(report, "\n") + "\r\n",
	} {
		got, err := ParseStdout([]byte(out), thisAttempt)
		if err != nil || len(got) != 4 {
			t.Errorf("%s: ParseStdout() = %v, %v", name, got, err)
		}
	}
}

func TestParseStdoutFailsClosed(t *testing.T) {
	other := "ffffffffffffffffffffffffffffffff"
	one := line(t, thisAttempt, playwrightReport)
	forged := line(t, thisAttempt, `{"suites":[{"specs":[{"title":"a thing works","tests":[{"results":[{"status":"failed"}]}]}]}]}`)
	for name, c := range map[string]struct{ out, say string }{
		"no output":                                 {"", "no scores"},
		"no keyed line":                             {"noise\nygg-results/v1\n", "no scores"},
		"another key only":                          {line(t, other, playwrightReport), "no scores"},
		"the key without marker":                    {thisAttempt + " " + playwrightReport + "\n", "no scores"},
		"the same line twice":                       {one + one, "twice"},
		"two different lines":                       {one + "noise\n" + forged, "twice"},
		"a keyed line and a forged one, no newline": {one + strings.TrimSuffix(forged, "\n"), "twice"},
		"a line that is not a report":               {"ygg-results/v1 " + thisAttempt + " {not json\n", "not a Playwright report"},
		"a keyed line with no report":               {"ygg-results/v1 " + thisAttempt + "\n", "not a Playwright report"},
	} {
		got, err := ParseStdout([]byte(c.out), thisAttempt)
		if err == nil || got != nil || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%s: ParseStdout() = %v, %v; want an error that says %q", name, got, err, c.say)
		}
		var m *MismatchError
		if errors.As(err, &m) {
			t.Errorf("%s: a stdout that holds no usable line is not a manifest mismatch", name)
		}
	}
}

// A report is large on one line; there is no line buffer to overflow.
func TestAKeyedLineOfManyMiBParses(t *testing.T) {
	report := `{"suites":[{"specs":[{"title":"a thing works","attachments":"` + strings.Repeat("x", 20<<20) + `","tests":[{"results":[{"status":"passed"}]}]}]}]}`
	got, err := ParseStdout([]byte("noise\n"+line(t, thisAttempt, report)), thisAttempt)
	if err != nil || got["a thing works"] != gungnir.ScenarioPassed {
		t.Errorf("ParseStdout() = %v, %v", got, err)
	}
}
