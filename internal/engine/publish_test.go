package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/views"
)

// The typed outputs of the planning stages, as a test's planner hands them back.
const (
	designDoc = `{"title": "Photo Share", "slug": "photo-share", "summary": "Albums of photos.", "goals": "Keep photos.", "non_goals": "No editing.", ` +
		`"success_criteria": "A page of 50 photos shows.", "context": "A family.", "product_at_user_level": "A person adds photos.", "decisions": [], ` +
		`"open_questions": [{"id": "q1", "question": "May guests download photos?", "who_can_answer": "The owner"}]}`
	specDoc = `{"title": "Photo Share", "slug": "photo-share", "summary": "An API and a web app.", "architecture": "Two leaves.", "interfaces": "POST /api/albums.", ` +
		`"data_model": "An album has a name.", "acceptance_criteria": "An album is made.", "security": "The owner writes.", "testing": "Unit tests.", ` +
		`"decisions": [], "deviations": [], "leaves": {"runtime": {"realm": "niflheim-local", "realm_version": "0.1.0"}, "leaves": {` +
		`"repos/api": {"realm": "helheim-go", "realm_version": "0.1.0", "description": "The API."}, ` +
		`"repos/web": {"realm": "midgard-react", "realm_version": "0.1.0", "description": "The web app.", "consumes": ["repos/api"]}}}}`
	reuseDoc = `{"verdict": "Remove or change 1 thing.", "items": [{"task": "plan-T001", "adds": "A retry library.", "already_done_by": "The context package.", "smallest_change": "Drop it."}]}`
)

const publishNodes = "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.design/v1 }\n  - { id: design-gate, kind: gate, gate: design }\n" +
	"  - { id: design-docs, kind: publish, from: design, to: docs }\n"

// publishScript is a designer that also leaves a draft in out/, which is never published.
const publishScript = "steps:\n  design:\n    - actions:\n        - write: { path: out/1-product/design.md, content: \"# A draft\\n\" }\n" +
	"      result: { status: ok, summary: wrote the design, output: '" + designDoc + "' }\n"

// The paths of the design's documents in the project, on the test's day.
const (
	designJSON = "docs/1-product/design/2026-10-24-photo-share.json"
	designMD   = "docs/1-product/design/2026-10-24-photo-share.md"
)

// onDay sets the run's clock to a day of its own: the date of every published document.
func onDay(r *run.Run) {
	r.Setup.Clock = func() time.Time { return time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC) }
}

func headFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, "show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// publishCommits maps each publish commit's node to the files it holds.
func publishCommits(t *testing.T, r *run.Run) map[string]string {
	t.Helper()
	out, err := git.Run(bg, r.Project, "log", "--format=%x00%s", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]string{}
	for _, c := range strings.Split(out, "\x00")[1:] {
		subject, files, _ := strings.Cut(c, "\n")
		if _, node, ok := strings.Cut(subject, ", node "); ok && strings.HasPrefix(subject, "Publish ") {
			commits[strings.TrimSuffix(node, ")")] = strings.TrimSpace(files)
		}
	}
	return commits
}

func readProject(t *testing.T, r *run.Run, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Project, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublishWritesTheJSONAndItsView(t *testing.T) {
	nodes := publishNodes +
		"  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [design, rejection], output: ygg.spec/v1 }\n" +
		"  - { id: spec-docs, kind: publish, from: spec, to: docs }\n" +
		"  - { id: plan, kind: agent, role: planner, prompt: p.md, scratch: true, input: [spec, rejection], output: ygg.plan/v1 }\n" +
		"  - { id: reuse, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: plan, input: [rejection], output: ygg.reuse-review/v1 }\n" +
		"  - { id: plan-docs, kind: publish, from: plan, to: docs }\n" +
		"  - { id: reuse-docs, kind: publish, from: reuse, to: docs }\n"
	script := publishScript +
		"  spec:\n    - result: { status: ok, summary: wrote the spec, output: '" + specDoc + "' }\n" +
		"  plan:\n    - result: { status: ok, summary: wrote the plan, output: '" + planDone + "' }\n" +
		"  reuse:\n    - result: { status: ok, summary: reviewed the plan, output: '" + reuseDoc + "' }\n"
	r := start(t, "unattended", "gates:\n  design: auto\n", nodes, script)
	onDay(r)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	const (
		specJSON  = "docs/2-engineering/specs/2026-10-24-photo-share.json"
		specMD    = "docs/2-engineering/specs/2026-10-24-photo-share.md"
		plans     = "docs/2-engineering/plans/2026-10-24-plan/"
		reuseJSON = "docs/2-engineering/reviews/2026-10-24-plan-reuse-review.json"
		reuseMD   = "docs/2-engineering/reviews/2026-10-24-plan-reuse-review.md"
	)
	// Each publish commits the output's JSON and its view, by their paths, and nothing of out/.
	want := map[string]string{
		"design-docs": designJSON + "\n" + designMD,
		"spec-docs":   "docs/2-engineering/specs/2026-10-24-photo-share.json\ndocs/2-engineering/specs/2026-10-24-photo-share.md\ndocs/leaves.yaml",
		"plan-docs":   plans + "plan.json\n" + plans + "plan.md\n" + plans + "tasks.md",
		"reuse-docs":  reuseJSON + "\n" + reuseMD,
	}
	if got := publishCommits(t, r); !reflect.DeepEqual(got, want) {
		t.Errorf("the publish commits hold %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(r.Project, "docs", "1-product", "design.md")); err == nil {
		t.Error("the draft in out/ was published")
	}
	if st, _ := git.Run(bg, r.Project, "status", "--porcelain", "--", "docs"); st != "" {
		t.Errorf("docs/ is left uncommitted: %q", st)
	}
	// The JSON is the output Odin kept, byte for byte; the view is rendered from it.
	for node, file := range map[string]string{"design": designJSON, "spec": specJSON, "plan": plans + "plan.json", "reuse": reuseJSON} {
		kept, err := os.ReadFile(filepath.Join(r.Dir, "outputs", node+".json"))
		if err != nil || string(readProject(t, r, file)) != string(kept) {
			t.Errorf("%s holds %q; want the output of %s Odin kept, %q (%v)", file, readProject(t, r, file), node, kept, err)
		}
	}
	design := decodeDoc[schema.Design](t, schema.ContractDesign, designDoc)
	spec := decodeDoc[schema.Spec](t, schema.ContractSpec, specDoc)
	plan := decodeDoc[schema.Plan](t, schema.ContractPlan, planDone)
	reuse := decodeDoc[schema.ReuseReview](t, schema.ContractReuseReview, reuseDoc)
	toSpec := []string{"../../specs/2026-10-24-photo-share.md"}
	for file, render := range map[string]func() ([]byte, error){
		designMD: func() ([]byte, error) {
			return views.Render(schema.ContractDesign, design, views.Front{Title: "Photo Share design", Date: "2026-10-24", Related: []string{}})
		},
		specMD: func() ([]byte, error) {
			return views.Render(schema.ContractSpec, spec, views.Front{Title: "Photo Share spec", Date: "2026-10-24", Related: []string{"../../1-product/design/2026-10-24-photo-share.md"}})
		},
		plans + "plan.md": func() ([]byte, error) {
			return views.Render(schema.ContractPlan, plan, views.Front{Title: "Plan plan", Date: "2026-10-24", Related: toSpec})
		},
		plans + "tasks.md": func() ([]byte, error) {
			return views.Tasks(plan, views.Front{Title: "Plan tasks", Date: "2026-10-24", Related: toSpec})
		},
		reuseMD: func() ([]byte, error) {
			return views.Render(schema.ContractReuseReview, reuse, views.Front{Title: "Plan reuse review", Date: "2026-10-24", Related: []string{"../plans/2026-10-24-plan/plan.md"}})
		},
	} {
		view, err := render()
		if err != nil {
			t.Fatal(err)
		}
		if got := readProject(t, r, file); string(got) != string(view) {
			t.Errorf("%s =\n%s\nwant the view of its output:\n%s", file, got, view)
		}
	}
	// leaves.yaml is the spec's typed leaves, as ParseLeaves reads them back.
	leaves, err := schema.ParseLeaves("docs/leaves.yaml", readProject(t, r, "docs/leaves.yaml"))
	if err != nil || !reflect.DeepEqual(*leaves, spec.Leaves) {
		t.Errorf("docs/leaves.yaml reads back as %+v, %v; want the spec's leaves %+v", leaves, err, spec.Leaves)
	}
	// Until the tasks node reads the plan's output (typed-T18), it reads tasks.md: the view holds every task.
	fs, err := schema.ParseTasks("tasks.md", readProject(t, r, plans+"tasks.md"))
	if err != nil || len(fs) != len(plan.Tasks) {
		t.Fatalf("tasks.md parses as %+v, %v", fs, err)
	}
	for i, f := range schema.FeaturesFromPlan(plan) {
		f.Text, f.Contract, f.Line = "", "", fs[i].Line
		if len(f.DependsOn) == 0 && len(fs[i].DependsOn) == 0 {
			f.DependsOn = fs[i].DependsOn // none is none, a nil list or an empty one
		}
		if !reflect.DeepEqual(fs[i], f) {
			t.Errorf("tasks.md gives task %+v, want %+v", fs[i], f)
		}
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"docs_published"`) || !strings.Contains(text, "2 file(s) under docs: "+designJSON) {
		t.Errorf("the ledger lacks docs_published:\n%s", text)
	}
}

func decodeDoc[T any](t *testing.T, c schema.Contract, doc string) T {
	t.Helper()
	v, err := schema.Decode[T](c, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPublishingTheSameDocumentsAgainCommitsNothing(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	onDay(r)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	n := r.Nodes()[2]
	if err := runPublish(bg, r, n); err != nil {
		t.Fatal(err)
	}
	if again, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); again != head {
		t.Error("publishing identical documents made a commit")
	}
	if got := r.State.Summaries["design-docs"]; !strings.Contains(got, "unchanged") {
		t.Errorf("summary = %q", got)
	}
}

func TestPublishRefusesABlockedOutput(t *testing.T) {
	script := "steps:\n  design:\n    - result: { status: ok, summary: could not, output: '{\"blocked\": {\"reason\": \"The brief is empty.\"}}' }\n"
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, script)
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "design is blocked: The brief is empty.") {
		t.Fatalf("Advance() = %+v, %v; want the publish failed on the blocked design", out, err)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after != head {
		t.Error("a blocked design was published")
	}
}

// A publish node publishes an output that has a view; any other is refused when the run starts, not when the
// node is reached.
func TestAPublishNodeWithNoViewIsRefusedWhenTheRunStarts(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, scratch: true, input: [rejection], output: ygg.implement/v1 }\n" +
		"  - { id: a-docs, kind: publish, from: a, to: docs }\n"
	_, err := run.Start(bg, startOptions(t, setup{nodes: nodes, script: "steps: {}\n"}))
	if err == nil || !strings.Contains(err.Error(), "node a-docs: publish has no view for ygg.implement/v1") {
		t.Fatalf("Start() = %v; want the publish node refused", err)
	}
}

func TestPublishWontCommitOverStagedWorkItDidNotMake(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	os.WriteFile(filepath.Join(r.Project, "notes.txt"), []byte("mine\n"), 0o644)
	if _, err := git.Run(bg, r.Project, "add", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "staged changes Odin didn't make") {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if files := headFiles(t, r.Project); strings.Contains(files, "notes.txt") {
		t.Error("the operator's staged file was committed with the documents")
	}
}

func TestSecretsInTheDocumentsAreAHardStopAndNothingIsPublished(t *testing.T) {
	r, fake := startSandboxed(t, publishNodes, publishScript)
	onDay(r)
	fake.Scan.Exit, fake.Scan.Report = 7, secretReport
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateRealSecrets || out.Gate != "design-docs" {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at real-secrets-or-user-data", out, err, r.State.PendingGate)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after != head {
		t.Error("documents with a secret in them were committed")
	}
	if _, err := os.Stat(filepath.Join(r.Project, "docs")); err == nil {
		t.Error("documents with a secret in them were copied into the project")
	}
	if len(fake.ScanFiles) != 1 || strings.Join(fake.ScanFiles[0], ",") != designJSON+","+designMD {
		t.Errorf("the scan was given %q; want the two documents, at their paths in the project", fake.ScanFiles)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "design-docs-1.md"))
	if !strings.Contains(string(b), "Nothing was published") || !strings.Contains(string(b), "outputs/design.json") {
		t.Errorf("the gate file = %s", b)
	}
	fake.Scan.Exit, fake.Scan.Report = 0, ""
	if err := Approve(bg, r, "design-docs", "a test key, removed"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after approval = %+v, %v", out, err)
	}
	if got := headFiles(t, r.Project); !strings.Contains(got, designMD) {
		t.Errorf("after approval the commit holds %q", got)
	}
}

// What is published is what the secret scan read: Odin reads the kept output once, gives the scan those bytes
// and the view it rendered from them, and writes those bytes, so a file changed after the scan read it is not what
// lands in the project (review M-2).
func TestPublishCommitsTheBytesTheScanRead(t *testing.T) {
	r, fake := startSandboxed(t, publishNodes, publishScript)
	onDay(r)
	scanned := map[string]string{}
	fake.OnScan = func(env gungnir.Env, c gungnir.Cmd, files []string) {
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(env.Workdir, c.Dir, f))
			if err != nil {
				t.Errorf("the scan can't read %s: %v", f, err)
			}
			scanned[f] = string(b)
		}
		if err := os.WriteFile(filepath.Join(r.Dir, "outputs", "design.json"), []byte(`{"changed": "after the scan"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := scanned[designJSON]; got != designDoc {
		t.Errorf("the scan read %q", got)
	}
	for _, f := range []string{designJSON, designMD} {
		if b := readProject(t, r, f); scanned[f] == "" || string(b) != scanned[f] {
			t.Errorf("published %q; the scan read %q", b, scanned[f])
		}
	}
}
