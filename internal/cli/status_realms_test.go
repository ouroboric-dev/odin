package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

func TestStatusShowsEachLeafsRealmAndTheTasksStartingWithTheOnesGivenUp(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--realm", toyRealm(t)); code != ExitStopped {
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
	for _, want := range []string{"realm:    toy 0.3.0 (graph 3.0.0)", "leaf:     repos/app on ygg/" + id, "(toy 0.3.0)",
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
	for _, w := range []string{"ERROR realms: asgard:", "ERROR realms: " + filepath.Join(store, "midgard-vue") + " holds the realm midgard-react, so --realm midgard-vue can't find it"} {
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
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); !strings.Contains(out, "WARN  realms: no realms section") {
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
