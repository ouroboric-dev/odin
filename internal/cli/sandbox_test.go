package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
)

const sandboxExtra = "sandbox:\n  engine: unix:///run/ygg-test.sock\n"

// stubEngine puts a podman and a devcontainer that log their calls first on PATH. The engine tools get
// none of the test's variables but PATH and the folders, so what they answer is in files beside them,
// which stubSays writes: rootless and left.
func stubEngine(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("podman", `d=$(dirname "$0")
echo "podman $*" >> "$d/calls.log"
answer() { if [ -f "$d/$1" ]; then cat "$d/$1"; else echo "$2"; fi; }
case "$1" in
  info) echo "$(answer rootless true) 5.7.1" ;;
  ps)
    case "$*" in
      *--format*) answer left "[]" ;;
      *) echo c1 ;;
    esac ;;
  rm) [ -f "$d/fail-rm" ] && exit 1 ;;
  pod)
    case "$2" in
      ls) answer pods "[]" ;;
      rm) [ -f "$d/fail-pod-rm" ] && echo "pod is busy" >&2 && exit 1 ;;
    esac ;;
  network) [ "$2" = exists ] && exit 1 ;;
  image) case "$2 $3" in
      "exists localhost/ygg-stack-"*) [ -f "$d/built-$(echo "$3" | tr '/:' '__')" ] || exit 1 ;;
    esac ;;
  build) case "$*" in *-t\ localhost/ygg-factory-stack:*) [ -f "$d/fail-factory" ] && exit 1 ;; esac
    prev=; for a in "$@"; do
      [ "$prev" = -t ] && case "$a" in localhost/ygg-stack-*) touch "$d/built-$(echo "$a" | tr '/:' '__')" ;; esac
      prev=$a
    done ;;
esac
exit 0
`)
	write("devcontainer", "echo 0.89.0\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

// stubSays sets what the stub engine whose log is log answers for name.
func stubSays(t *testing.T, log, name, answer string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(log), name), []byte(answer+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withSandbox returns a copy of config with a sandbox section, for doctor: a run starts under the plain
// config (the toy realm has no sandbox stack), and doctor then checks it against the sandboxed one.
func withSandbox(t *testing.T, config string) string {
	t.Helper()
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(p, append(b, sandboxExtra...), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func leftoverJSON(run, leaf, project string) string {
	return `[{"Id":"c1c1c1c1c1c1c1","State":"running","Labels":{"dev.ouroboric.ygg.run":"` + run + `","dev.ouroboric.ygg.leaf":"` + leaf + `","com.docker.compose.project":"` + project + `"}}]`
}

func TestDoctorChecksTheEngineAndStopsLeftoverSandboxes(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	// A run of this project that no ygg holds: its sandbox is a leftover.
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", plain, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	id := runID(t, dir)
	stubSays(t, log, "left", leftoverJSON(id, "repos/app", "ygg-p1"))
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	for _, want := range []string{"OK    engine: podman 5.7.1, rootless", "OK    devcontainer: CLI 0.89.0", "WARN  sandboxes: run " + id + " left the sandbox of repos/app (running); stopped it"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor's output lacks %q:\n%s", want, out)
		}
	}
	if code != ExitDone {
		t.Errorf("exit %d: a leftover that doctor stopped isn't an error", code)
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "podman rm -f -t 2 --depend c1") {
		t.Errorf("doctor never stopped the leftover container:\n%s", got)
	}
}

func TestDoctorLeavesWhatALiveRunOrAnotherProjectOwns(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	if code, _, _ := ygg(t, "run", "--project", dir, "--config", plain, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatal("run")
	}
	id := runID(t, dir)
	release, err := lock.Take(filepath.Join(run.Dir(dir, id), "lock")) // a live ygg holds the run
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stubSays(t, log, "left", leftoverJSON(id, "repos/app", "ygg-live"))
	ygg(t, "doctor", "--project", dir, "--config", config)
	stubSays(t, log, "left", leftoverJSON("20250101-000000-abcdef", "repos/app", "ygg-other"))
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if !strings.Contains(out, "belongs to another project") {
		t.Errorf("doctor's output:\n%s", out)
	}
	if got, _ := os.ReadFile(log); strings.Contains(string(got), "podman rm") {
		t.Errorf("doctor stopped a container a live run or another project owns:\n%s", got)
	}
}

// podJSON lists empty pods in the shape of podman pod ls, one per name.
func podJSON(names ...string) string {
	var rows []string
	for _, n := range names {
		rows = append(rows, `{"Name":"`+n+`","Status":"Created","Containers":[]}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// podOf is the name of the pod compose made for a sandbox of a run.
func podOf(id string) string { return "pod_" + sandbox.RunPrefix(id) + "reposapp00112233" }

// stoppedRun makes a run of the project that no ygg holds and returns its id.
func stoppedRun(t *testing.T, dir, plain string) string {
	t.Helper()
	if code, _, errOut := ygg(t, "run", "--project", dir, "--config", plain, "--graph", toyRealm(t)); code != ExitStopped {
		t.Fatalf("run: exit %d: %s", code, errOut)
	}
	return runID(t, dir)
}

func TestDoctorRemovesTheEmptyPodsOfRunsNoYggHolds(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	id := stoppedRun(t, dir, plain)
	stubSays(t, log, "pods", podJSON(podOf(id)))
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	for _, want := range []string{"WARN  pods: run " + id + " left the empty pod " + podOf(id) + " (Created); removed it", "OK    pods: 1 empty pod(s) of ygg runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor's output lacks %q:\n%s", want, out)
		}
	}
	if code != ExitDone {
		t.Errorf("exit %d: a pod that doctor removed isn't an error", code)
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "podman pod rm "+podOf(id)) {
		t.Errorf("doctor never removed the pod:\n%s", got)
	}
}

func TestDoctorLeavesThePodOfALiveRunAndOfAnotherProject(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	id := stoppedRun(t, dir, plain)
	release, err := lock.Take(filepath.Join(run.Dir(dir, id), "lock")) // a live ygg holds the run
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stubSays(t, log, "pods", podJSON(podOf(id), podOf("20250101-000000-abcdef")))
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "pod "+podOf("20250101-000000-abcdef")+" belongs to another project") || !strings.Contains(out, "OK    pods: 2 empty pod(s)") {
		t.Errorf("doctor's output:\n%s", out)
	}
	if got, _ := os.ReadFile(log); strings.Contains(string(got), "pod rm") {
		t.Errorf("doctor removed a pod a live run or another project owns:\n%s", got)
	}
}

func TestDoctorReportsAPodItCannotRemoveAndGoesOn(t *testing.T) {
	log := stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	id := stoppedRun(t, dir, plain)
	stubSays(t, log, "pods", podJSON(podOf(id)))
	stubSays(t, log, "fail-pod-rm", "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone {
		t.Errorf("exit %d: a pod that can't be removed isn't a doctor failure:\n%s", code, out)
	}
	for _, want := range []string{"WARN  pods: couldn't remove " + podOf(id) + " of run " + id + ": ", "pod is busy", "OK    pods: 1 empty pod(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor's output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorSaysNothingOfPodsWhenThereAreNone(t *testing.T) {
	stubEngine(t)
	dir, plain := project(t, "script.yaml", "")
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", withSandbox(t, plain))
	if strings.Contains(out, "pods") {
		t.Errorf("doctor's output:\n%s", out)
	}
}

func TestDoctorRefusesARootfulEngine(t *testing.T) {
	stubSays(t, stubEngine(t), "rootless", "false")
	dir, plain := project(t, "script.yaml", "")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", withSandbox(t, plain))
	if code != ExitFailed || !strings.Contains(out, "ERROR engine:") || !strings.Contains(out, "rootful") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestDoctorSaysWhenThereIsNoSandbox(t *testing.T) {
	dir, config := project(t, "script.yaml", "")
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if !strings.Contains(out, "WARN  sandbox: no sandbox section") {
		t.Errorf("doctor's output:\n%s", out)
	}
}

func TestSandboxBuildPullsAndBuilds(t *testing.T) {
	log := stubEngine(t)
	_, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	realm, _ := filepath.Abs("../../testdata/toy-sandbox")
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", realm)
	if code != ExitDone || !strings.Contains(out, "localhost/ygg-work:") || !strings.Contains(out, "localhost/ygg-fw:") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"podman pull -q --authfile", "--pull=never --target fw", "--pull=never --target work"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the engine calls lack %q:\n%s", want, got)
		}
	}
	for name, args := range map[string][]string{
		"no realm":                    {"sandbox", "build", "--config", config},
		"an unknown verb":             {"sandbox", "push"},
		"a toy realm without a stack": {"sandbox", "build", "--config", config, "--realm", toyRealm(t)},
	} {
		if code, _, _ := ygg(t, args...); code != ExitRefused {
			t.Errorf("%s: exit %d, want %d", name, code, ExitRefused)
		}
	}
}

func TestSandboxBuildAlsoBuildsTheFactoryLayer(t *testing.T) {
	log := stubEngine(t)
	_, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	realm, _ := filepath.Abs("../../testdata/toy-sandbox")
	if code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", realm); code != ExitDone {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	b, _ := os.ReadFile(log)
	factory := sandbox.FactoryImages()
	at := func(substr string) (found []int) {
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "podman build") && strings.Contains(line, substr) {
				found = append(found, i)
			}
		}
		return found
	}
	// The factory stack is built from the pinned Node image, then the layer on it, once; both before the realm's stack.
	stack, layer := at("-t "+sandbox.FactoryStack+" "), at("-t "+factory.Work+" ")
	if len(stack) != 1 || len(layer) != 1 || stack[0] > layer[0] {
		t.Fatalf("the factory stack was built at %v and its layer at %v; want once each, stack first:\n%s", stack, layer, b)
	}
	var realmLayers []int
	for _, i := range at("--target work") {
		if i != layer[0] {
			realmLayers = append(realmLayers, i)
		}
	}
	if len(realmLayers) != 1 || realmLayers[0] < layer[0] {
		t.Errorf("the realm's layer was built at %v, the factory layer at %v; the factory layer comes first:\n%s", realmLayers, layer, b)
	}
}

func TestSandboxBuildStopsWhenTheFactoryLayerFailsToBuild(t *testing.T) {
	log := stubEngine(t)
	stubSays(t, log, "fail-factory", "yes")
	_, plain := project(t, "script.yaml", "")
	config := withSandbox(t, plain)
	realm, _ := filepath.Abs("../../testdata/toy-sandbox")
	code, _, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", realm)
	if code != ExitFailed || !strings.Contains(errOut, "factory layer") {
		t.Errorf("exit %d, want %d with the factory layer named:\n%s", code, ExitFailed, errOut)
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "podman build") && !strings.Contains(line, "ygg-factory-stack") {
			t.Errorf("a build ran after the factory stack failed: %s", line)
		}
		if strings.Contains(line, "golang:") {
			t.Errorf("the realm's stack was pulled after the factory layer failed: %s", line)
		}
	}
}

// The runtime realm names no stack and starts the services the build realms' leaves need: ygg sandbox build --all
// pulls its service images, which a sandbox never pulls itself, and still lists it as a realm with no stack.
func TestSandboxBuildPullsTheRuntimesServices(t *testing.T) {
	log := stubEngine(t)
	store, config := realmsStore(t)
	const (
		pg18 = "docker.io/library/postgres:18.1@sha256:4444444444444444444444444444444444444444444444444444444444444444"
		s3   = "docker.io/chrislusf/seaweedfs:4.45@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5"
	)
	pack := "name: niflheim-local\nversion: 0.1.0\nservices:\n  postgres:\n    image: " + pg18 + "\n    ports: [5432]\n" +
		"  s3:\n    image: " + s3 + "\n    ports: [8333]\nprovides:\n  postgres: postgres\n  s3: s3\n"
	if err := os.MkdirAll(filepath.Join(store, "niflheim-local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "niflheim-local", "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
	_, images, skipped, err := stacksToBuild(&schema.Factory{Realms: &schema.Realms{Dir: store}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{pg18, s3} {
		if !slices.Contains(images, want) {
			t.Errorf("the images to pull are %v; they lack the runtime realm's %s", images, want)
		}
	}
	if !slices.ContainsFunc(skipped, func(s string) bool { return strings.Contains(s, "niflheim-local names no sandbox stack") }) {
		t.Errorf("skipped = %v; want niflheim-local listed as a realm with no stack", skipped)
	}
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--all")
	if code != ExitDone || !strings.Contains(out, "Pulling the service image "+s3) {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "pull ") || !strings.Contains(string(b), s3) {
		t.Errorf("the engine was not asked to pull %s:\n%s", s3, b)
	}
}

// A realm with no stack but with provides, named on purpose, gives its service images, as --all does.
func TestSandboxBuildRealmWithNoStackPullsItsServices(t *testing.T) {
	log := stubEngine(t)
	store, config := realmsStore(t)
	const s3 = "docker.io/chrislusf/seaweedfs:4.45@sha256:fc9f76fa993ad69966ffeb2f65d0318fcae39c6f8e20cf68ef7b3a5cb97769e5"
	pack := "name: niflheim-local\nversion: 0.1.0\nservices:\n  s3:\n    image: " + s3 + "\n    ports: [8333]\nprovides:\n  s3: s3\n"
	if err := os.MkdirAll(filepath.Join(store, "niflheim-local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "niflheim-local", "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", "niflheim-local")
	if code != ExitDone || !strings.Contains(out, "Pulling the service image "+s3) {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), s3) {
		t.Errorf("the engine was not asked to pull %s:\n%s", s3, b)
	}
}

// A build realm named with --realm that declares needs says that its providers' images come from the runtime
// realm, and which command pulls them.
func TestSandboxBuildRealmWithNeedsSaysWhereTheServicesAre(t *testing.T) {
	stubEngine(t)
	store, config := realmsStore(t)
	pack := "name: helheim-api\nversion: 0.1.0\nneeds: [postgres, s3]\nsandbox:\n  stack: " + goStack + "\n"
	if err := os.MkdirAll(filepath.Join(store, "helheim-api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "helheim-api", "pack.yaml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "sandbox", "build", "--config", config, "--realm", "helheim-api")
	if code != ExitDone || !strings.Contains(out, "The realm helheim-api needs postgres, s3") || !strings.Contains(out, "name the runtime realm with --realm too, or use --all") {
		t.Fatalf("exit %d; the output lacks where the needs' services are pulled:\n%s%s", code, out, errOut)
	}
}
