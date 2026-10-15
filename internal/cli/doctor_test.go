package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsMissingCaps(t *testing.T) {
	dir, config := project(t, "script.yaml", "budget:\n  per_run: { tokens: 100000, turns: 100 }\n")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR caps:") || !strings.Contains(out, "budget.per_day") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
}

func TestDoctorFindsAChangedLedger(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t))
	id := runID(t, dir)
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); code != ExitDone || !strings.Contains(out, "OK    run "+id+": stopped") {
		t.Fatalf("doctor on a good run: exit %d:\n%s", code, out)
	}
	path := filepath.Join(dir, ".factory", "runs", id, "ledger.jsonl")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(b, []byte(`"gen_ai.usage.input_tokens":1000`), []byte(`"gen_ai.usage.input_tokens":1`), 1), 0o644)
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR run "+id+": ledger:") {
		t.Errorf("doctor on a changed ledger: exit %d:\n%s", code, out)
	}
}
