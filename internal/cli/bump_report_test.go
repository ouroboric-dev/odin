package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
)

const bumpID = "20261021-090000-b0b0b0"

// plantBump writes the ledger of a bump of golang.org/x/text in repos/app: a line with no verdict says the
// tool was about to run, and a line with one says it ended.
func plantBump(t *testing.T, project, id string, verdicts ...string) {
	t.Helper()
	l, err := ledger.Open(filepath.Join(project, ".factory", "deps", id, "ledger.jsonl"), ledger.Anchor{})
	if err != nil {
		if err := os.MkdirAll(filepath.Join(project, ".factory", "deps", id), 0o755); err != nil {
			t.Fatal(err)
		}
		if l, err = ledger.Open(filepath.Join(project, ".factory", "deps", id, "ledger.jsonl"), ledger.Anchor{}); err != nil {
			t.Fatal(err)
		}
	}
	defer l.Close()
	for _, v := range verdicts {
		note := "repos/app: started"
		if v != "" {
			note = "repos/app"
		}
		if err := l.Append(gungnir.Event{Name: gungnir.EventDepsBump, Run: id, Package: "golang.org/x/text", From: "0.41.0", To: "0.42.0",
			Stage: "canary", Verdict: v, Note: note}); err != nil {
			t.Fatal(err)
		}
	}
}

// withLeaf makes the leaf repos/app, by a run that stops at its first gate.
func withLeaf(t *testing.T, dir, config string) {
	t.Helper()
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
}

func TestDoctorListsAnUnfinishedBump(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	withLeaf(t, dir, config)
	plantBump(t, dir, bumpID, "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	want := "WARN  bump " + bumpID + ": the bump of golang.org/x/text stopped in repos/app before it ended; the product may be changed. Run: git -C " + filepath.Join(dir, "repos/app") + " diff\nThen, once the product is as you want it, remove " + filepath.Join(dir, ".factory", "deps", bumpID)
	if !strings.Contains(out, want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
	if code != ExitDone {
		t.Errorf("exit %d: an unfinished bump is a warning, as a leftover sandbox is", code)
	}
}

func TestDoctorAndStatusAreSilentOnABumpThatEnded(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	plantBump(t, dir, bumpID, "", "pass")
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); strings.Contains(out, "bump") {
		t.Errorf("doctor on a bump that ended:\n%s", out)
	}
	if code, _, _ := ygg(t, "run", "--project", dir, "--config", config, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatal("run")
	}
	if _, out, _ := ygg(t, "status", runID(t, dir), "--project", dir); strings.Contains(out, "bump") {
		t.Errorf("status on a bump that ended:\n%s", out)
	}
}

func TestDoctorSkipsABumpThatAYggHolds(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	plantBump(t, dir, bumpID, "")
	release, err := lock.Take(run.RunsLock(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, out, _ := ygg(t, "doctor", "--project", dir, "--config", config); strings.Contains(out, "bump "+bumpID) {
		t.Errorf("doctor listed a bump that is running:\n%s", out)
	}
}

func TestDoctorStopsTheContainersOfABump(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	withLeaf(t, dir, plain)
	plantBump(t, dir, bumpID, "", "pass")
	stubSays(t, log, "left", leftoverJSON(bumpID, "repos/app", "ygg-bump"))
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	want := "WARN  sandboxes: bump " + bumpID + " left the sandbox of repos/app (running); stopped it. Run: git -C " + filepath.Join(dir, "repos/app") + " diff\nThen, once the product is as you want it, remove " + filepath.Join(dir, ".factory", "deps", bumpID)
	if !strings.Contains(out, want) {
		t.Errorf("doctor lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "belongs to another project") {
		t.Errorf("doctor took a bump's container for another project's:\n%s", out)
	}
	if code != ExitDone {
		t.Errorf("exit %d", code)
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "podman rm -f -t 2 --depend c1") {
		t.Errorf("doctor never stopped the container:\n%s", got)
	}
}

func TestDoctorLeavesTheContainersOfARunningBump(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	plantBump(t, dir, bumpID, "")
	release, err := lock.Take(run.RunsLock(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stubSays(t, log, "left", leftoverJSON(bumpID, "repos/app", "ygg-bump"))
	ygg(t, "doctor", "--project", dir, "--config", config)
	if got, _ := os.ReadFile(log); strings.Contains(string(got), "podman rm") {
		t.Errorf("doctor stopped the container of a bump that is running:\n%s", got)
	}
}

func TestStatusListsAnUnfinishedBumpAndItsContainers(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", plain, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: %s", errOut)
	}
	id := runID(t, dir)
	_, out, _ := ygg(t, "status", id, "--project", dir)
	if strings.Contains(out, "bump") {
		t.Errorf("status with no bump:\n%s", out)
	}
	plantBump(t, dir, bumpID, "")
	stubSays(t, log, "left", leftoverJSON(bumpID, "repos/app", "ygg-bump"))
	// status reads the sandbox section of the run's own config
	b, _ := os.ReadFile(plain)
	if err := os.WriteFile(plain, append(b, sandboxExtra...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = ygg(t, "status", id, "--project", dir)
	if strings.Contains(out, "git -C") {
		t.Errorf("status offered git while a container may hold the product:\n%s", out)
	}
	for _, want := range []string{
		"bump:     " + bumpID + ": the bump of golang.org/x/text stopped in repos/app before it ended; the product may be changed. A container of its sandbox may still hold the product, so git is not safe there yet. Run: ygg doctor --project " + dir,
		"bump:     " + bumpID + ": a container of the sandbox of repos/app is left (running). Run: ygg doctor --project " + dir,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorAndStatusDoNotOfferGitInAProductWhoseGitWasChanged(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", plain, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: %s", errOut)
	}
	plantBump(t, dir, bumpID, "")
	stubSays(t, log, "left", leftoverJSON(bumpID, "repos/app", "ygg-bump"))
	product := filepath.Join(dir, "repos/app")
	if err := os.WriteFile(filepath.Join(product, ".git"), []byte("gitdir: /tmp/elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "Do not run git in " + product + ". Once the product is as you want it, remove " + filepath.Join(dir, ".factory", "deps", bumpID)
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if strings.Count(out, "The .git of the product is not as it was") != 2 || strings.Count(out, want) != 2 || strings.Contains(out, "Run: git -C") {
		t.Errorf("doctor, ledger and container:\n%s", out)
	}
	b, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, append(b, sandboxExtra...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = ygg(t, "status", runID(t, dir), "--project", dir)
	if strings.Contains(out, "Do not run git") || strings.Contains(out, "Run: git -C") {
		t.Errorf("status:\n%s", out)
	}
}

func TestDoctorOffersNoGitWhileTheContainersOfABumpAreNotStopped(t *testing.T) {
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	withLeaf(t, dir, plain)
	plantBump(t, dir, bumpID, "")
	for name, setup := range map[string]func(log string){
		"the container list fails": func(log string) { stubSays(t, log, "left", "not json") },
		"the stop fails": func(log string) {
			stubSays(t, log, "left", leftoverJSON(bumpID, "repos/app", "ygg-bump"))
			stubSays(t, log, "fail-rm", "x")
		},
	} {
		setup(stubEngine(t))
		_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
		if strings.Contains(out, "git -C") || strings.Contains(out, "stopped it") {
			t.Errorf("%s: doctor offered git:\n%s", name, out)
		}
		if want := "bump " + bumpID + ": the bump of golang.org/x/text stopped in repos/app before it ended; the product may be changed. The sandbox containers of the bump are not known to be stopped, so git is not safe in the product yet. Once the engine answers, run: ygg doctor --project " + dir; !strings.Contains(out, want) {
			t.Errorf("%s: doctor lacks %q:\n%s", name, want, out)
		}
	}
}
