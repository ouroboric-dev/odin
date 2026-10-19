package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
)

const (
	toyStack = "docker.io/library/golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514"
	toyPG    = "docker.io/library/postgres:17.2@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	toyS3    = "docker.io/chrislusf/seaweedfs:3.80@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// toySandboxBlock is the sandbox lines of a realm's pack.yaml: a stack, an acceptance stack when withAcceptance,
// and the registries it opens.
func toySandboxBlock(withAcceptance bool, egress string) string {
	b := "sandbox:\n  stack: " + toyStack + "\n"
	if withAcceptance {
		b += "  acceptance: { stack: " + acceptanceStack + " }\n"
	}
	return b + "  egress: [" + egress + "]\n"
}

// acceptanceRealm is a realm with the unit check of the toy leaves, a sandbox, and the acceptance check
// accept when it is not empty.
func acceptanceRealm(name, accept string, withAcceptance bool, egress, services string) realmFiles {
	pack := "checks:\n  unit: [grep, -q, done, build.txt]\n"
	if accept != "" {
		pack += "  accept: " + accept + "\n"
	}
	return realmFiles{name: name, version: "0.1.0", pack: pack + toySandboxBlock(withAcceptance, egress) + services,
		files: map[string]string{"p.md": "Do the work.\n"}}
}

const toyMoveNodes = "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/api }\n" +
	"  - { id: merge, kind: merge, repo: repos/api, checks: [unit] }\n" +
	"  - { id: move, kind: pointer, repo: repos/api, checks: [unit], acceptance: [accept] }\n" +
	"  - { id: other-touch, kind: exec, run: [echo, other], repo: repos/web }\n"

const webAccept = `[sh, -c, "grep -q done repos/api/build.txt && test -f repos/web/README.md"]`

// acceptanceLines are the check_result lines of the acceptance check accept, as (realm, result) pairs.
func acceptanceLines(t *testing.T, r *run.Run) [][2]string {
	t.Helper()
	var out [][2]string
	for _, line := range strings.Split(ledgerText(t, r), "\n") {
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw["event"] != "check_result" || !strings.HasPrefix(asString(raw["ygg.result"]), "accept:") {
			continue
		}
		out = append(out, [2]string{asString(raw["ygg.realm.name"]), asString(raw["ygg.result"])})
	}
	return out
}

func asString(v any) string { s, _ := v.(string); return s }

func TestEveryMoveRunsTheAcceptanceCheckOfEveryLeafRealmThatHasOne(t *testing.T) {
	apiRealm := acceptanceRealm("api-toy", "", false, "proxy.golang.org", "services:\n  seaweedfs:\n    image: "+toyS3+"\n    ports: [8333]\n")
	webRealm := acceptanceRealm("web-toy", webAccept, true, "registry.npmjs.org",
		"services:\n  postgres:\n    image: "+toyPG+"\n    env: { POSTGRES_DB: app }\n    ports: [5432]\n")
	setup := func(fake *sandboxfake.Sandbox) realmsSetup {
		return realmsSetup{mode: "unattended", nodes: toyMoveNodes, script: buildScript, realms: []realmFiles{apiRealm, webRealm},
			leaves: map[string]string{"repos/api": "api-toy", "repos/web": "web-toy"}, sandbox: fake}
	}

	t.Run("in a sandbox", func(t *testing.T) {
		fake := &sandboxfake.Sandbox{}
		r := startRealms(t, setup(fake))
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusDone {
			t.Fatalf("Advance() = %+v, %v", out, err)
		}
		var snaps []gungnir.SandboxSpec
		for _, u := range fake.Ups {
			if strings.HasPrefix(u.Name, "snapshot/") {
				snaps = append(snaps, u)
			}
		}
		if len(snaps) != 1 || snaps[0].Name != "snapshot/move/web-toy" {
			t.Fatalf("snapshot sandboxes = %+v; want one, snapshot/move/web-toy: api-toy has no acceptance check", snaps)
		}
		s := snaps[0]
		var services []string
		for _, v := range s.Services {
			services = append(services, v.Name)
		}
		if s.Stack != acceptanceStack || strings.Join(services, ",") != "postgres,seaweedfs" || strings.Join(s.Egress, ",") != "proxy.golang.org,registry.npmjs.org" ||
			!s.Scratch || s.Leaf != filepath.Join(r.Dir, "snapshot", "move", "web-toy") {
			t.Errorf("spec = %+v; want web-toy's acceptance stack, the services and registries of both realms", s)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("the spec is one the driver would refuse: %v", err)
		}
		for _, f := range []string{"repos/api/build.txt", "repos/web/README.md"} {
			if _, err := os.Stat(filepath.Join(s.Leaf, f)); err != nil {
				t.Errorf("the snapshot lacks %s: %v", f, err)
			}
		}
		got := acceptanceLines(t, r)
		if len(got) != 1 || got[0] != [2]string{"web-toy", "accept: exit 0"} {
			t.Errorf("acceptance lines = %v; want one, for web-toy", got)
		}
		if _, err := os.Stat(filepath.Join(r.Dir, "logs", "move-1-accept-web-toy.log")); err != nil {
			t.Errorf("the acceptance log is not at logs/<node>-<attempt>-<check>-<realm>.log: %v", err)
		}
		if !strings.Contains(ledgerText(t, r), `"event":"pointer_moved"`) {
			t.Error("the pointer did not move")
		}
	})

	t.Run("on the host", func(t *testing.T) {
		r := startRealms(t, setup(nil))
		out, err := Advance(bg, r)
		if err != nil || out.Status != run.StatusDone {
			t.Fatalf("Advance() = %+v, %v", out, err)
		}
		got := acceptanceLines(t, r)
		if len(got) != 1 || got[0] != [2]string{"web-toy", "accept: exit 0"} {
			t.Errorf("acceptance lines = %v; a run with no sandbox runs the same check on the host", got)
		}
	})
}

func TestAcceptanceRunsInRealmNameOrderAndStopsAtTheFirstFailure(t *testing.T) {
	nodes := strings.ReplaceAll(strings.ReplaceAll(toyMoveNodes, "repos/api", "repos/a"), "repos/web", "repos/b")
	start := func(aAccept string) (*run.Run, *sandboxfake.Sandbox) {
		fake := &sandboxfake.Sandbox{}
		// b-toy is the graph pack, so the realms are listed b first; the order of the run is by name.
		return startRealms(t, realmsSetup{mode: "unattended", nodes: nodes, script: buildScript,
			realms: []realmFiles{acceptanceRealm("b-toy", `["true"]`, true, "proxy.golang.org", ""), acceptanceRealm("a-toy", aAccept, true, "registry.npmjs.org", "")},
			leaves: map[string]string{"repos/a": "a-toy", "repos/b": "b-toy"}, sandbox: fake}), fake
	}
	snapshots := func(fake *sandboxfake.Sandbox) []string {
		var names []string
		for _, u := range fake.Ups {
			if strings.HasPrefix(u.Name, "snapshot/") {
				names = append(names, u.Name)
			}
		}
		return names
	}

	r, fake := start(`["true"]`)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	got := acceptanceLines(t, r)
	if len(got) != 2 || got[0] != [2]string{"a-toy", "accept: exit 0"} || got[1] != [2]string{"b-toy", "accept: exit 0"} {
		t.Errorf("acceptance lines = %v; want a-toy then b-toy", got)
	}
	if s := snapshots(fake); strings.Join(s, ",") != "snapshot/move/a-toy,snapshot/move/b-toy" {
		t.Errorf("snapshot sandboxes = %v; each realm has its own", s)
	}

	r, fake = start(`["false"]`)
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "check accept") {
		t.Fatalf("Advance() = %+v, %v; want the move refused on a-toy's acceptance check", out, err)
	}
	got = acceptanceLines(t, r)
	if len(got) != 1 || got[0][0] != "a-toy" || got[0][1] == "accept: exit 0" {
		t.Errorf("acceptance lines = %v; want only a-toy's failure, with no b-toy line", got)
	}
	if s := snapshots(fake); strings.Join(s, ",") != "snapshot/move/a-toy" {
		t.Errorf("snapshot sandboxes = %v; b-toy's must not come up after a-toy failed", s)
	}
	if strings.Contains(ledgerText(t, r), `"event":"pointer_moved"`) {
		t.Error("the pointer moved although acceptance failed")
	}
}

// taskShapeWithAcceptance is the task shape whose pointer move asks for the acceptance check accept.
var taskShapeWithAcceptance = strings.Replace(taskShape, "      - { id: move, kind: pointer, realm: leaf }\n", "      - { id: move, kind: pointer, realm: leaf, acceptance: [accept] }\n", 1)

// initAcceptanceRun starts a run on a project with no leaves, whose graph makes the leaves and then moves
// each task's pointer with the acceptance check accept, and writes the leaves file into it.
func initAcceptanceRun(t *testing.T, sandbox *sandboxfake.Sandbox, realms ...realmFiles) *run.Run {
	t.Helper()
	r := startRealms(t, realmsSetup{mode: "unattended", nodes: initNode + taskShapeWithAcceptance, script: "steps: {}\n", realms: realms, sandbox: sandbox})
	if err := os.MkdirAll(filepath.Join(r.Project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	leaves := "leaves:\n  repos/api:\n    realm: api-toy\n    realm_version: 0.1.0\n    description: The API.\n"
	if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(leaves), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

// initRefused advances r and checks that init refused with want before it made anything.
func initRefused(t *testing.T, r *run.Run, want string) {
	t.Helper()
	umbrella := filepath.Join(r.Project, "umbrella.yaml")
	before, err := os.ReadFile(umbrella)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), want) {
		t.Fatalf("Advance() = %+v, %v; want init refused with %q", out, err, want)
	}
	if strings.Contains(ledgerText(t, r), `"event":"repo_created"`) {
		t.Error("a leaf was created before init refused")
	}
	if after, _ := os.ReadFile(umbrella); string(after) != string(before) {
		t.Errorf("umbrella.yaml changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(r.Project, "repos", "api")); err == nil {
		t.Error("repos/api exists")
	}
}

func TestAProjectWhereNoLeafRealmHasTheAcceptanceCheckIsRefused(t *testing.T) {
	const want = "no realm of this project has an acceptance check"
	t.Run("Prepare, when the project has leaves", func(t *testing.T) {
		r, err := tryRealms(t, realmsSetup{mode: "unattended", nodes: taskShapeWithAcceptance, script: "steps: {}\n",
			realms: []realmFiles{recipeRealm("api-toy", "0.1.0", "api.txt", "API."), recipeRealm("web-toy", "0.1.0", "web.txt", "Web.")},
			leaves: map[string]string{"repos/api": "api-toy", "repos/web": "web-toy"}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Start() = %v, %v; want a refusal: %s", r, err, want)
		}
	})
	t.Run("init, before any leaf is created", func(t *testing.T) {
		initRefused(t, initAcceptanceRun(t, nil, recipeRealm("api-toy", "0.1.0", "api.txt", "API.")), want)
	})
	t.Run("init, when another leaf's realm has the check", func(t *testing.T) {
		web := recipeRealm("web-toy", "0.1.0", "web.txt", "Web.")
		web.pack = strings.Replace(web.pack, "checks:\n", "checks:\n  accept: [\"true\"]\n", 1)
		r := initAcceptanceRun(t, nil, recipeRealm("api-toy", "0.1.0", "api.txt", "API."), web)
		// The leaves file names the api leaf and a web leaf, so one realm of the project has the check.
		leaves := "leaves:\n  repos/api:\n    realm: api-toy\n    realm_version: 0.1.0\n    description: The API.\n" +
			"  repos/web:\n    realm: web-toy\n    realm_version: 0.1.0\n    description: The web app.\n"
		if err := os.WriteFile(filepath.Join(r.Project, "docs", "leaves.yaml"), []byte(leaves), 0o644); err != nil {
			t.Fatal(err)
		}
		// The run goes on to the tasks node, which has no tasks file: the failure is that one, not a refusal.
		out, err := Advance(bg, r)
		if err != nil || out.Err == nil || strings.Contains(out.Err.Error(), want) || !strings.Contains(out.Err.Error(), "node ready") {
			t.Fatalf("Advance() = %+v, %v; want init past its check and the run at the tasks node", out, err)
		}
		if n := strings.Count(ledgerText(t, r), `"event":"repo_created"`); n != 2 {
			t.Errorf("%d repo_created lines, want 2", n)
		}
	})
}

func TestARealmWithAnAcceptanceCheckAndNoAcceptanceStackIsRefusedBeforeTheRunStarts(t *testing.T) {
	const want = "names no acceptance stack"
	noStack := func() realmFiles { return acceptanceRealm("api-toy", `["true"]`, false, "proxy.golang.org", "") }
	t.Run("Prepare", func(t *testing.T) {
		fake := &sandboxfake.Sandbox{}
		r, err := tryRealms(t, realmsSetup{mode: "unattended", nodes: strings.Replace(toyMoveNodes, "  - { id: other-touch, kind: exec, run: [echo, other], repo: repos/web }\n", "", 1), script: buildScript, realms: []realmFiles{noStack()},
			leaves: map[string]string{"repos/api": "api-toy"}, sandbox: fake})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "api-toy") {
			t.Fatalf("Start() = %v, %v; want a refusal naming api-toy: %s", r, err, want)
		}
		if len(fake.Ups) != 0 {
			t.Error("a sandbox came up before the refusal")
		}
	})
	t.Run("init", func(t *testing.T) {
		fake := &sandboxfake.Sandbox{}
		api := noStack()
		api.pack += "leaf:\n  description: the api leaf\n  template: templates/leaf\n  first_commit: [LICENSE]\n  implementer: prompts/implementer.md\n  checks: [unit]\n"
		api.files["templates/leaf/LICENSE"] = "licence\n"
		api.files["prompts/implementer.md"] = "API.\n"
		initRefused(t, initAcceptanceRun(t, fake, api), want)
		if len(fake.Ups) != 0 {
			t.Error("a sandbox came up before the refusal")
		}
	})
}
