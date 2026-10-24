package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// The exit criterion of Phase 5b, with scripted agents: the real graph pack software (Voluspa) and the real
// realms midgard-react and helheim-go build a product from a brief, end to end, through Odin's real code: the
// graph and prompts of the graph pack, the templates and leaf recipes of the realms. Only two things are not
// the realms': the agents, which answer from a script, and the check ladder's commands, which are replaced by
// plain ones because the ladder's tools live in the stack images (the ladder runs on a real engine, v0-H14
// and v0-H15). The test needs voluspa and the realms beside odin, as they are in the yggdrasil umbrella; it
// skips without them.

// realRealm copies the realm repo at src into store/name, leaving out git, packages and build output, and
// swaps the ladder's scripts for commands any machine runs.
func realRealm(t *testing.T, store, name string) {
	t.Helper()
	dst := copyBesideOdin(t, store, name)
	pack := filepath.Join(dst, "pack.yaml")
	b, _ := os.ReadFile(pack)
	plain := regexp.MustCompile(`\[/opt/ygg/checks/accept-e2e\.sh\]`).ReplaceAllString(string(b), "[test, -f, umbrella.yaml]")
	plain = regexp.MustCompile(`\[/opt/ygg/checks/[a-z0-9-]+\.sh\]`).ReplaceAllString(plain, "[test, -f, AGENTS.md]")
	if plain == string(b) {
		t.Fatalf("%s: no check to replace", pack)
	}
	if err := os.WriteFile(pack, []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyBesideOdin copies the repo ../../../name into store/name, leaving out git, packages and build output, and
// returns the copy. It skips the test when the repo is not beside odin.
func copyBesideOdin(t *testing.T, store, name string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "..", name))
	if err == nil {
		src, err = filepath.EvalSymlinks(src) // beside odin, the repo may be a link (a plan's reference workspace)
	}
	if err != nil {
		t.Skipf("%s is not beside odin: %v", name, err)
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		t.Skipf("%s is not beside odin (%s); run this test in the yggdrasil umbrella", name, src)
	}
	dst := filepath.Join(store, name)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.Name() == ".git" { // a folder, or the file a submodule has: a copy is a plain folder
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "dist") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

const leafReviewers = "  gallery-t%d-implement-spec-compliance:\n    - result: { status: ok, report: [] }\n  gallery-t%d-implement-simplicity:\n    - result: { status: ok, report: [] }\n" +
	"  gallery-t%d-implement-quality:\n    - result: { status: ok, report: [] }\n"

const realScript = `steps:
  design:
    - actions: [ { write: { path: out/1-product/design/2026-10-17-gallery.md, content: "---\ntitle: Gallery design\nstatus: draft\ndate: 2026-10-17\nrelated: []\n---\n# Gallery design\n" } } ]
      result: { status: ok, summary: wrote the design, usage: { in: 1000, out: 100, turns: 2 }, output: '{"summary": "done"}' }
  spec:
    - actions:
        - write: { path: out/2-engineering/specs/2026-10-17-gallery.md, content: "---\ntitle: Gallery spec\nstatus: draft\ndate: 2026-10-17\nrelated: []\n---\n# Gallery spec\n" }
        - write:
            path: out/leaves.yaml
            content: |
              runtime:
                realm: niflheim-local
                realm_version: NIFLHEIM_VERSION
              leaves:
                repos/api:
                  realm: helheim-go
                  realm_version: HELHEIM_VERSION
                  description: The gallery's API.
                repos/web:
                  realm: midgard-react
                  realm_version: MIDGARD_VERSION
                  description: The gallery's web app.
                  consumes: [repos/api]
      result: { status: ok, summary: wrote the spec and named the leaves, usage: { in: 1000, out: 100, turns: 2 }, output: '{"summary": "done"}' }
  conformance:
    - actions: [ { write: { path: out/conformance.json, content: '{"deviation": false}' } } ]
      result: { status: ok, summary: the spec stays inside the design, usage: { in: 500, out: 50, turns: 1 }, output: '{"deviation": false}' }
  plan:
    - actions:
        - write:
            path: out/2-engineering/plans/2026-10-17-gallery/tasks.md
            content: |
              ---
              title: Gallery tasks
              status: draft
              date: 2026-10-17
              related: []
              ---
              # Tasks

              ## gallery-T1: the API stores galleries

              - **Acceptance criteria:**
                - [ ] a gallery can be created
              - **Named test:** gallery_test.go TestCreateGallery
              - **Layer:** repos/api
              - **Size:** S
              - **Depends on:** nothing

              ## gallery-T2: the web app lists them

              - **Acceptance criteria:**
                - [ ] the home page lists galleries
              - **Named test:** e2e/galleries.spec.ts
              - **Layer:** repos/web
              - **Size:** M
              - **Depends on:** gallery-T1

              ## gallery-T3: the web app shows a gallery

              - **Acceptance criteria:**
                - [ ] a gallery page shows its name
              - **Named test:** src/gallery.test.tsx
              - **Layer:** repos/web
              - **Size:** S
              - **Depends on:** gallery-T2
        - write:
            path: out/2-engineering/plans/2026-10-17-gallery/plan.md
            content: "---\ntitle: Gallery plan\nstatus: draft\ndate: 2026-10-17\nrelated: []\n---\n# Gallery plan\n"
      result: { status: ok, summary: wrote the plan, usage: { in: 1000, out: 100, turns: 2 }, output: '` + planDone + `' }
  reuse-review:
    - actions: [ { write: { path: out/2-engineering/reviews/2026-10-17-gallery-reuse-review.md, content: "---\ntitle: Gallery reuse review\nstatus: draft\ndate: 2026-10-17\nrelated: []\n---\n## Verdict\n\nNothing to remove.\n" } } ]
      result: { status: ok, summary: nothing to remove, usage: { in: 500, out: 50, turns: 1 }, output: '{"summary": "done"}' }
  gallery-t1-implement:
    - actions: [ { write: { path: api.txt, content: "done\n" } } ]
      result: { status: ok, summary: built the API's galleries, usage: { in: 2000, out: 200, turns: 4 } }
  gallery-t2-implement:
    - actions: [ { write: { path: web.txt, content: "done\n" } } ]
      result: { status: ok, summary: built the page, usage: { in: 2000, out: 200, turns: 4 }, output: '{"summary": "done"}' }
  gallery-t3-implement:
    - actions: [ { write: { path: gallery.txt, content: "done\n" } } ]
      result: { status: ok, summary: built the gallery page, usage: { in: 2000, out: 200, turns: 4 }, output: '{"summary": "done"}' }
`

func TestTheRealRealmsBuildAProductFromABriefWithScriptedAgents(t *testing.T) {
	store := t.TempDir()
	realRealm(t, store, "midgard-react")
	realRealm(t, store, "helheim-go")
	copyBesideOdin(t, store, "niflheim-local")          // the runtime realm: it has no check to replace
	graphs := copyBesideOdin(t, t.TempDir(), "voluspa") // the graph pack's repo: graphs.dir holds software
	versions := map[string]string{}
	for _, name := range []string{"midgard-react", "helheim-go", "niflheim-local"} {
		p, err := schema.LoadPack(filepath.Join(store, name))
		if err != nil {
			t.Fatalf("the realm %s does not load: %v", name, err)
		}
		versions[name] = p.Version
		if name != "niflheim-local" && p.Leaf == nil {
			t.Fatalf("%s offers no leaf recipe", name)
		}
	}
	gp, err := schema.LoadPack(filepath.Join(graphs, "software"))
	if err != nil || gp.Graph == "" {
		t.Fatalf("the software graph pack does not load: %+v, %v", gp, err)
	}
	// The spec prompt offers each realm at the version it has: a spec that names another fails the leaves check.
	specPrompt, _ := os.ReadFile(filepath.Join(graphs, "software", "prompts", "spec.md"))
	for name, v := range versions {
		if !strings.Contains(string(specPrompt), "`"+name+"` "+v) {
			t.Errorf("the software graph's spec prompt does not offer %s %s", name, v)
		}
	}
	script := strings.NewReplacer("HELHEIM_VERSION", versions["helheim-go"], "NIFLHEIM_VERSION", versions["niflheim-local"], "MIDGARD_VERSION", versions["midgard-react"]).Replace(realScript)
	for i := 1; i <= 3; i++ {
		script += fmt.Sprintf(leafReviewers, i, i, i)
	}
	path := filepath.Join(t.TempDir(), "script.yaml")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	roles := "{ agent: scripted, model: none, script: " + path + " }"
	cfg := filepath.Join(t.TempDir(), "factory.yaml")
	body := "mode: key-points\nroles:\n  planner: " + roles + "\n  implementer: " + roles + "\n  reviewer: " + roles + "\n" +
		"budget:\n  per_run: { tokens: 1000000, turns: 1000 }\n  per_day: { tokens: 2000000, turns: 2000 }\nrealms:\n  dir: " + store + "\ngraphs:\n  dir: " + graphs + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := treetest.Project(t)
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "software")
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point design-gate") {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	id := runID(t, dir)
	if code, out, errOut = ygg(t, "approve", id, "design-gate", "--project", dir); code != ExitDone || !strings.Contains(out, "is done.") {
		t.Fatalf("approve: exit %d:\n%s%s", code, out, errOut)
	}

	for _, f := range []string{"docs/1-product/design/2026-10-17-gallery.md", "docs/2-engineering/specs/2026-10-17-gallery.md", "docs/leaves.yaml",
		"docs/2-engineering/plans/2026-10-17-gallery/tasks.md", "docs/2-engineering/plans/2026-10-17-gallery/plan.md", "docs/2-engineering/reviews/2026-10-17-gallery-reuse-review.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s was not published: %v", f, err)
		}
	}
	st, err := run.LoadState(run.Dir(dir, id))
	if err != nil || st.Status != run.StatusDone {
		t.Fatalf("run.json: %+v, %v", st, err)
	}
	if _, ok := st.Realms["niflheim-local"]; !ok {
		t.Errorf("run.json records the realms %v, want niflheim-local among them", st.Realms)
	}
	// The leaves were made from the real templates of their realms.
	for leaf, files := range map[string][]string{"repos/api": {"openapi.yaml", "sqlc.yaml", "internal/store/migrate.go", "cmd/api/main.go"},
		"repos/web": {"package.json", "package-lock.json", "src/lib/api.ts", "contracts/api.openapi.yaml", "e2e/smoke.spec.ts"}} {
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(dir, leaf, f)); err != nil {
				t.Errorf("%s/%s: %v; the leaf is made from the template of its realm", leaf, f, err)
			}
		}
	}
	u, err := schema.LoadUmbrella(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Graph != "software" || u.Children["repos/api"].Realm != "helheim-go" || u.Children["repos/web"].Realm != "midgard-react" {
		t.Errorf("the project's pins: graph %s; api %+v; web %+v", u.Graph, u.Children["repos/api"], u.Children["repos/web"])
	}
	for leaf, f := range map[string]string{"repos/api": "api.txt", "repos/web": "gallery.txt"} {
		if body, err := git.Run(ctx, filepath.Join(dir, leaf), "show", "main:"+f); err != nil || body != "done" {
			t.Errorf("%s: main:%s = %q, %v", leaf, f, body, err)
		}
		main, _ := git.Run(ctx, filepath.Join(dir, leaf), "rev-parse", "main")
		if got, _ := git.Run(ctx, dir, "rev-parse", "HEAD:"+leaf); got != main {
			t.Errorf("the project points at %s for %s, want main %s", got, leaf, main)
		}
	}
	feats, err := schema.LoadFeatures(filepath.Join(dir, ".factory", "features.json"))
	if err != nil || len(feats.Tasks) != 3 {
		t.Fatalf("features.json = %+v, %v", feats, err)
	}
	for _, f := range feats.Tasks {
		if f.Status != schema.TaskDone {
			t.Errorf("task %s is %s", f.ID, f.Status)
		}
	}
	// The three reviewers of the graph ran for every task, in the task's leaf, and the conformance review ran.
	for i := 1; i <= 3; i++ {
		for _, rv := range []string{"spec-compliance", "simplicity", "quality"} {
			key := fmt.Sprintf("gallery-t%d-implement-%s", i, rv)
			if st.Attempts[key] != 1 {
				t.Errorf("reviewer session %s ran %d times, want 1", key, st.Attempts[key])
			}
		}
	}
	ledger, _ := os.ReadFile(filepath.Join(run.Dir(dir, id), "ledger.jsonl"))
	for _, want := range []string{`"event":"conformance_checked"`, `"ygg.result":"no deviation"`, `"event":"tasks_ready"`, `"event":"pointer_moved"`} {
		if !strings.Contains(string(ledger), want) {
			t.Errorf("the ledger lacks %s", want)
		}
	}
	// The simplicity reviewer and the reuse review got the vendored Ponytail skill in their prompts.
	for _, stem := range []string{"reuse-review-1", "gallery-t1-implement-simplicity-1"} {
		tr, err := os.ReadFile(filepath.Join(run.Dir(dir, id), "transcripts", stem+".json"))
		if err != nil || !strings.Contains(string(tr), "Review a change like the senior developer who will be paged") {
			t.Errorf("transcript %s lacks the Ponytail skill: %v", stem, err)
		}
	}
	_, status, _ := ygg(t, "status", id, "--project", dir)
	for _, want := range []string{"graph:    software " + gp.Version, "(helheim-go " + versions["helheim-go"] + ")", "tasks:    3 (3 done, 0 doing, 0 to do, 0 blocked)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
}
