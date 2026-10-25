package cli

import (
	"encoding/json"
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

// leafReviewers are the answers of the three reviewers of task %03d's loop: ygg.review/v1 documents with no finding.
const leafReviewers = "  gallery-t%03[1]d-implement-spec-compliance:\n    - result: { status: ok, output: '{\"findings\": []}' }\n" +
	"  gallery-t%03[1]d-implement-simplicity:\n    - result: { status: ok, output: '{\"findings\": []}' }\n" +
	"  gallery-t%03[1]d-implement-quality:\n    - result: { status: ok, output: '{\"findings\": []}' }\n"

// The typed answers of the scripted stages (change typed-boundaries): each is the whole final answer of its session,
// a document of the contract its node declares, in a single-quoted YAML scalar (so it holds no apostrophe). The design asks one open question, which the design gate answers, and
// the spec cites the owner decision given there.
const (
	galleryDesign = `{"title": "Gallery", "slug": "gallery", "summary": "Families keep photo galleries.", "goals": "A family makes a gallery and sees it.", ` +
		`"non_goals": "No photo editing.", "success_criteria": "The home page lists the galleries.", "context": "A family shares photos.", ` +
		`"product_at_user_level": "A person makes a gallery, finds it on the home page and opens it.", "decisions": [], ` +
		`"open_questions": [{"id": "q1", "question": "May guests download photos?", "who_can_answer": "The owner"}]}`
	gallerySpec = `{"title": "Gallery", "slug": "gallery", "summary": "An API that stores galleries and a web app that shows them.", ` +
		`"architecture": "repos/api serves /api/galleries; repos/web calls it.", "interfaces": "GET and POST /api/galleries.", ` +
		`"data_model": "A gallery has an id and a name.", "acceptance_criteria": "A gallery made through the API is listed and shown.", ` +
		`"security": "Only the family writes.", "testing": "Unit tests in each leaf and an end-to-end test.", ` +
		`"decisions": [{"date": "2026-10-17", "decision": "Guests may download photos.", "reason": "The owner decided it.", "cites": ["guest-download"]}], ` +
		`"deviations": [], "leaves": {"runtime": {"realm": "niflheim-local", "realm_version": "NIFLHEIM_VERSION"}, "leaves": {` +
		`"repos/api": {"realm": "helheim-go", "realm_version": "HELHEIM_VERSION", "description": "The API of the gallery."}, ` +
		`"repos/web": {"realm": "midgard-react", "realm_version": "MIDGARD_VERSION", "description": "The web app of the gallery.", "consumes": ["repos/api"]}}}}`
	galleryContract = `openapi: 3.1.0\ninfo: { title: Gallery, version: 0.1.0 }\npaths:\n  /api/galleries: {}\n`
	galleryPlan     = `{"title": "Gallery", "slug": "gallery", "goal": "Galleries are stored, listed and shown.", "decisions": [], "risks": [], "tasks": [` +
		`{"id": "gallery-T001", "title": "The API stores galleries", "criteria": ["A gallery can be created.", "It is listed."], ` +
		`"named_test": "gallery_test.go TestCreateGallery", "layer": "repos/api", "size": "S", "depends_on": [], ` +
		`"text": "POST /api/galleries stores a gallery with a name; GET lists them.", "contract": "` + galleryContract + `"}, ` +
		`{"id": "gallery-T002", "title": "The web app lists them", "criteria": ["The home page lists galleries.", "An empty list says so."], ` +
		`"named_test": "e2e/galleries.spec.ts", "layer": "repos/web", "size": "M", "depends_on": ["gallery-T001"], ` +
		`"text": "The home page calls GET /api/galleries and lists each name.", "contract": "` + galleryContract + `"}, ` +
		`{"id": "gallery-T003", "title": "The web app shows a gallery", "criteria": ["A gallery page shows its name.", "An unknown gallery shows an error."], ` +
		`"named_test": "src/gallery.test.tsx", "layer": "repos/web", "size": "S", "depends_on": ["gallery-T002"], ` +
		`"text": "A gallery page shows the name of the gallery."}]}`
)

const realScript = `steps:
  design:
    - result: { status: ok, summary: wrote the design, usage: { in: 1000, out: 100, turns: 2 }, output: '` + galleryDesign + `' }
  spec:
    - result: { status: ok, summary: wrote the spec and named the leaves, usage: { in: 1000, out: 100, turns: 2 }, output: '` + gallerySpec + `' }
  conformance:
    - result: { status: ok, summary: the spec stays inside the design, usage: { in: 500, out: 50, turns: 1 }, output: '{"deviation": false}' }
  plan:
    - result: { status: ok, summary: wrote the plan, usage: { in: 1000, out: 100, turns: 2 }, output: '` + galleryPlan + `' }
  reuse-review:
    - result: { status: ok, summary: nothing to remove, usage: { in: 500, out: 50, turns: 1 }, output: '{"verdict": "Nothing to remove.", "items": []}' }
  gallery-t001-implement:
    - actions: [ { write: { path: api.txt, content: "done\n" } } ]
      result: { status: ok, summary: built the API's galleries, usage: { in: 2000, out: 200, turns: 4 }, output: '{"summary": "done"}' }
  gallery-t002-implement:
    - actions: [ { write: { path: web.txt, content: "done\n" } } ]
      result: { status: ok, summary: built the page, usage: { in: 2000, out: 200, turns: 4 }, output: '{"summary": "done"}' }
  gallery-t003-implement:
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
		script += fmt.Sprintf(leafReviewers, i)
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
	brief := writeFile(t, "brief.md", "Build a photo gallery for a family: an API that stores galleries and a web app that lists and shows them.\n")
	code, out, errOut := ygg(t, "run", "--project", dir, "--config", cfg, "--graph", "software", "--brief", brief)
	if code != ExitStopped || !strings.Contains(out, "stopped at approval point design-gate") {
		t.Fatalf("run: exit %d:\n%s%s", code, out, errOut)
	}
	id := runID(t, dir)
	// The owner decides at the design gate and answers the design's question: the spec must cite the decision.
	decisions := writeFile(t, "decisions.json", `{"decisions": [{"id": "guest-download", "text": "Guests may download photos."}], `+
		`"answers": [{"question": "q1", "text": "Yes, guests may."}]}`)
	if code, out, errOut = ygg(t, "approve", id, "design-gate", "--project", dir, "--decisions", decisions); code != ExitDone || !strings.Contains(out, "is done.") {
		t.Fatalf("approve: exit %d:\n%s%s", code, out, errOut)
	}

	// Each stage's document is published as its JSON and its view, named by the run's date and the slug.
	for _, f := range []string{"docs/1-product/design/*-gallery.json", "docs/1-product/design/*-gallery.md", "docs/2-engineering/specs/*-gallery.json",
		"docs/2-engineering/specs/*-gallery.md", "docs/leaves.yaml", "docs/2-engineering/plans/*-gallery/plan.json", "docs/2-engineering/plans/*-gallery/tasks.md",
		"docs/2-engineering/plans/*-gallery/plan.md", "docs/2-engineering/reviews/*-gallery-reuse-review.md"} {
		if m, _ := filepath.Glob(filepath.Join(dir, f)); len(m) != 1 {
			t.Errorf("%s was not published once: %q", f, m)
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
			key := fmt.Sprintf("gallery-t%03d-implement-%s", i, rv)
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
	for _, stem := range []string{"reuse-review-1", "gallery-t001-implement-simplicity-1"} {
		tr, err := os.ReadFile(filepath.Join(run.Dir(dir, id), "transcripts", stem+".json"))
		if err != nil || !strings.Contains(string(tr), "Review a change like the senior developer who will be paged") {
			t.Errorf("transcript %s lacks the Ponytail skill: %v", stem, err)
		}
	}
	// The spec was told the owner's decision and the answer, with its question, and cited the decision.
	spec, err := os.ReadFile(filepath.Join(run.Dir(dir, id), "transcripts", "spec-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr struct{ Prompt string }
	if err := json.Unmarshal(spec, &tr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Input: decisions", "- guest-download (at design-gate): Guests may download photos.",
		"- q1 (at design-gate): Yes, guests may.\n  The question: May guests download photos?", "## Input: brief\n\nBuild a photo gallery for a family"} {
		if !strings.Contains(tr.Prompt, want) {
			t.Errorf("the spec's prompt lacks %q:\n%s", want, tr.Prompt)
		}
	}
	if st.Returns["spec"] != 0 {
		t.Errorf("the owner check sent the spec back %d time(s); it cites the one decision", st.Returns["spec"])
	}
	_, status, _ := ygg(t, "status", id, "--project", dir)
	for _, want := range []string{"graph:    software " + gp.Version, "(helheim-go " + versions["helheim-go"] + ")", "tasks:    3 (3 done, 0 doing, 0 to do, 0 blocked)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
}
