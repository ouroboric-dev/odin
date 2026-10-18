package score

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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

func TestReadResultsReadsNestedSuitesAndTheLastTryOfEachTest(t *testing.T) {
	got, err := ReadResults(write(t, playwrightReport))
	if err != nil {
		t.Fatalf("ReadResults() = %v", err)
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

func TestReadResultsRefusesWhatItCannotReadExactly(t *testing.T) {
	var m *MismatchError
	_, err := ReadResults(write(t, `{"suites":[{"specs":[{"title":"x","tests":[]},{"title":"x","tests":[]}]}]}`))
	if !errors.As(err, &m) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("a spec title used twice: %v", err)
	}
	for name, body := range map[string]string{"not json": `{"suites":`, "an empty file": ``, "no suites": `{"suites":[]}`} {
		if _, err := ReadResults(write(t, body)); err == nil {
			t.Errorf("%s: ReadResults() = nil", name)
		}
	}
	if _, err := ReadResults(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing file read")
	}
}

func TestAFailedLastTryIsAFailureWhateverCameBefore(t *testing.T) {
	body := `{"suites":[{"specs":[
	 {"title":"passes then fails","tests":[{"results":[{"status":"passed"},{"status":"failed"}]}]},
	 {"title":"interrupted","tests":[{"results":[{"status":"interrupted"}]}]},
	 {"title":"a status nobody knows","tests":[{"results":[{"status":"flaky-ish"}]}]},
	 {"title":"ran in one project, skipped in another","tests":[{"results":[{"status":"passed"}]},{"results":[{"status":"skipped"}]}]},
	 {"title":"no tests","tests":[]}]}]}`
	got, err := ReadResults(write(t, body))
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
