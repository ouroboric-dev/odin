package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

func TestStatusShowsEachLeafsRealmAndTheTasksStartingWithTheOnesGivenUp(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	id := runID(t, dir)
	st, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	f := func(id string, status schema.TaskStatus, why string) schema.FeatureRecord {
		return schema.FeatureRecord{Feature: schema.Feature{ID: id}, Status: status, Why: why}
	}
	st.Features = []schema.FeatureRecord{f("p-T1", schema.TaskDone, ""), f("p-T2", schema.TaskBlocked, "its loop gave up (attempts-exhausted)"), f("p-T3", schema.TaskTodo, "")}
	if err := st.Save(run.Dir(dir, id)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "status", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("status: exit %d: %s", code, errOut)
	}
	for _, want := range []string{"graph:    toy 0.3.0", "leaf:     repos/app on ygg/" + id, "(toy 0.3.0)",
		"blocked:  p-T2: its loop gave up (attempts-exhausted)", "tasks:    3 (1 done, 0 doing, 1 to do, 1 blocked)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "blocked:") > strings.Index(out, "leaf:") {
		t.Errorf("the blocked tasks should come before the leaves:\n%s", out)
	}
	if !strings.Contains(usage, "5 done, with tasks given up") || ExitBlocked != 5 {
		t.Error("the usage text lacks exit code 5")
	}
}

func TestDoctorListsTheRealmsOfTheStoreAndChecksEachLoads(t *testing.T) {
	stubEngine(t)
	store, config := realmsStore(t)
	dir, _ := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	want := "OK    realms: " + store + " (asgard 0.1.0, helheim-go 0.1.0, midgard-react 0.1.0, midgard-vue 0.1.0)"
	if !strings.Contains(out, want) {
		t.Errorf("doctor's output lacks %q:\n%s", want, out)
	}
	if code != ExitDone {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// A realm that doesn't load, and a folder that holds another realm than its name says.
	os.WriteFile(filepath.Join(store, "asgard", "pack.yaml"), []byte("name: asgard\n"), 0o644)
	os.WriteFile(filepath.Join(store, "midgard-vue", "pack.yaml"), []byte("name: midgard-react\nversion: 0.1.0\n"), 0o644)
	code, out, _ = ygg(t, "doctor", "--project", dir, "--config", config)
	for _, w := range []string{"ERROR realms: asgard:", "ERROR realms: " + filepath.Join(store, "midgard-vue") + " holds the realm midgard-react, so a project can't pin midgard-vue"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor's output lacks %q:\n%s", w, out)
		}
	}
	if code != ExitFailed {
		t.Errorf("exit %d; a realm that doesn't load is an error", code)
	}
}

func TestDoctorSaysWhenThereAreNoRealms(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); !strings.Contains(out, "WARN  realms: no realms section: a project can't pin a realm leaf") {
		t.Errorf("doctor's output:\n%s", out)
	}
	empty := t.TempDir()
	b, _ := os.ReadFile(config)
	cfg := filepath.Join(t.TempDir(), "factory.yaml")
	os.WriteFile(cfg, append(b, []byte("realms:\n  dir: "+empty+"\n")...), 0o644)
	if code, out, _ := ygg(t, "doctor", "--project", dir, "--config", cfg); code != ExitFailed || !strings.Contains(out, "ERROR realms: "+empty+" holds no realm leaf") {
		t.Errorf("an empty store: exit %d:\n%s", code, out)
	}
}

func TestStatusListsAnOverruledFindingWithBothSides(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	id := runID(t, dir)
	st, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	rec := &run.LoopRecord{State: *jormungandr.NewState(jormungandr.Config{Attempts: 3})}
	rec.State.Book.Records = map[string]*jormungandr.Record{"spec-1": {Finding: gungnir.Finding{ID: "spec-1", Source: "spec", Severity: gungnir.SeverityBlocker,
		Location: "app.js:4", Claim: "findIndex is not a JavaScript method", Status: gungnir.FindingOverruled,
		Proof: "Implementer's evidence: curl showed it is defined\nCheck unit ran and passed in attempt 2: check unit ran in attempt 2 and passed (logs/build-2-unit.log)"}, Raised: 1, Closed: 2}}
	st.Loops["audit"] = rec
	if err := st.Save(run.Dir(dir, id)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "status", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("status: exit %d: %s", code, errOut)
	}
	for _, want := range []string{"overruled: audit: spec-1 (blocker, from spec) at app.js:4", "reviewer's claim: findIndex is not a JavaScript method",
		"curl showed it is defined", "check unit ran in attempt 2 and passed (logs/build-2-unit.log)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

// A blocked task's reason, a pause's reason and a run's error are agent text: status prints each on one line,
// with no control character.
func TestStatusCleansAgentTextOnOneLine(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	id := runID(t, dir)
	st, err := run.LoadState(run.Dir(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	hostile := "bad\x1b[2J\u202eevil\nsecond line"
	st.Features = []schema.FeatureRecord{{Feature: schema.Feature{ID: "p-T2"}, Status: schema.TaskBlocked, Why: hostile}}
	st.Paused = &run.Pause{Node: "n", Since: time.Now(), Reason: hostile}
	st.Error = hostile
	if err := st.Save(run.Dir(dir, id)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "status", id, "--project", dir)
	if code != ExitDone {
		t.Fatalf("status: exit %d: %s", code, errOut)
	}
	if strings.ContainsAny(out, "\x1b\u202e") || strings.Contains(out, "second line") {
		t.Errorf("status prints agent text raw or on a second line:\n%q", out)
	}
	for _, want := range []string{"blocked:  p-T2: bad [2J evil\n", "error:    bad [2J evil\n", ": bad [2J evil\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%q", want, out)
		}
	}
}
