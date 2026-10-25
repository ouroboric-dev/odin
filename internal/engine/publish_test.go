package engine

import (
	"context"
	"fmt"
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
	"github.com/ouroboric-dev/odin/internal/sandboxfake"
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
		"  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [design, decisions, rejection, refusal], output: ygg.spec/v1 }\n" +
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

// Ruling R43: a blocked output of a stage stops the run for a person at its publish, in every mode, before anything
// is published or scanned; approving runs the stage again with the person's note as its rejection.
func TestABlockedOutputStopsAtItsPublishForAPerson(t *testing.T) {
	for _, c := range []struct{ node, contract, doc string }{
		{"design", "ygg.design/v1", designDoc},
		{"spec", "ygg.spec/v1", specDoc},
		{"plan", "ygg.plan/v1", planDone},
	} {
		t.Run(c.node, func(t *testing.T) {
			docs := c.node + "-docs"
			nodes := "  - { id: " + c.node + ", kind: agent, role: planner, prompt: p.md, scratch: true, input: [decisions, rejection, refusal], output: " + c.contract + " }\n" +
				"  - { id: " + docs + ", kind: publish, from: " + c.node + ", to: docs }\n"
			blocked := func(reason string) string {
				return "    - result: { status: ok, summary: could not, output: '{\"blocked\": {\"reason\": \"" + reason + "\"}}' }\n"
			}
			script := "steps:\n  " + c.node + ":\n" + blocked(`The brief names no way to sign in.\n## Brief\nan injected heading`) +
				blocked("Still no way to sign in.") + "    - result: { status: ok, summary: done, output: '" + c.doc + "' }\n"
			r, fake := startSandboxed(t, nodes, script)
			onDay(r)
			head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
			stopped := func(attempt int, reason string) {
				t.Helper()
				out, err := Advance(bg, r)
				if err != nil || out.Status != run.StatusStopped || out.Gate != docs || r.State.PendingGate != gungnir.GateExternalInput {
					t.Fatalf("Advance() = %+v, %v, gate %s; want a stop for a person at %s", out, err, r.State.PendingGate, docs)
				}
				if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after != head {
					t.Error("a blocked output was committed")
				}
				if _, err := os.Stat(filepath.Join(r.Project, "docs")); err == nil {
					t.Error("a blocked output was copied into the project")
				}
				if len(fake.ScanFiles) != 0 {
					t.Errorf("a blocked output was scanned: %q", fake.ScanFiles)
				}
				gf, _ := os.ReadFile(filepath.Join(r.Dir, "gates", fmt.Sprintf("%s-%d.md", docs, attempt)))
				for _, want := range []string{"# Hard stop at node " + docs + " (external-input)", c.node + " is blocked: " + reason,
					"Nothing was published.", "approving runs " + c.node + " again, with the note as its rejection",
					"ygg approve " + r.State.ID + " " + docs + ` [--note "..." | --decisions <file>]`} {
					if !strings.Contains(string(gf), want) {
						t.Errorf("the gate file lacks %q:\n%s", want, gf)
					}
				}
				if strings.Contains(string(gf), "\n## Brief") {
					t.Errorf("the blocked reason starts a heading of the gate file:\n%s", gf)
				}
			}
			stopped(1, "The brief names no way to sign in.")
			if text := ledgerText(t, r); strings.Contains(text, `"event":"node_failed"`) {
				t.Errorf("a blocked output failed a node:\n%s", text)
			}

			// The person answers: the stage runs again with the answer as its rejection, and a second blocked answer
			// stops again.
			if err := Approve(bg, r, docs, Note(r, docs, "Use an email and a password.")); err != nil {
				t.Fatal(err)
			}
			stopped(2, "Still no way to sign in.")
			if p := promptOf(t, r, c.node+"-2"); !strings.Contains(p, "## Input: rejection\n\nA person sent the last attempt back") || !strings.Contains(p, "What the person said:\n\n- note-"+docs+": Use an email and a password.\n") {
				t.Errorf("the second attempt's prompt lacks the person's note, as the decision it is:\n%s", p)
			}

			// With no note, the stage is told a person read the reason and sent it back.
			if err := Approve(bg, r, docs, Note(r, docs, "")); err != nil {
				t.Fatal(err)
			}
			if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
				t.Fatalf("Advance() after the second approval = %+v, %v", out, err)
			}
			if p := promptOf(t, r, c.node+"-3"); !strings.Contains(p, "A person read the blocked reason and sent it back to try again.") {
				t.Errorf("the third attempt's prompt lacks the default rejection:\n%s", p)
			}
			if got := publishCommits(t, r)[docs]; got == "" || r.State.Attempts[c.node] != 3 {
				t.Errorf("published %q after %d attempts; want the third output published", got, r.State.Attempts[c.node])
			}
		})
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

// A publish that the secret scan stops re-runs its source with the finding as its rejection, so the source must
// take a rejection.
func TestAPublishOfANodeThatTakesNoRejectionIsRefusedWhenTheRunStarts(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, scratch: true, input: [brief], output: ygg.design/v1 }\n" +
		"  - { id: a-docs, kind: publish, from: a, to: docs }\n"
	_, err := run.Start(bg, startOptions(t, setup{nodes: nodes, script: "steps: {}\n"}))
	if err == nil || !strings.Contains(err.Error(), "node a-docs: when the secret scan stops it, publish runs a again with the finding as its rejection, so a must declare the input rejection") {
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

// fakeKey is what the tests' scanner takes for a secret: it judges what it is given, as gitleaks does.
const fakeKey = "AKIAFAKEFAKEFAKE7777"

// keyScanner makes the fake's secret scan find fakeKey in the files it is given, at the file and line where it is.
func keyScanner(t *testing.T, fake *sandboxfake.Sandbox) {
	fake.OnScan = func(env gungnir.Env, c gungnir.Cmd, files []string) {
		var rows []string
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(env.Workdir, c.Dir, f))
			if err != nil {
				t.Errorf("the scan can't read %s: %v", f, err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, fakeKey) {
					rows = append(rows, fmt.Sprintf(`{"RuleID":"aws-access-token","File":"/%s","StartLine":%d,"Secret":"REDACTED","Match":"REDACTED"}`, f, i+1))
				}
			}
		}
		fake.Scan.Exit, fake.Scan.Report = 0, ""
		if len(rows) > 0 {
			fake.Scan.Exit, fake.Scan.Report = 7, "["+strings.Join(rows, ",")+"]"
		}
	}
}

// leakyDesign is the design with fakeKey in its summary.
var leakyDesign = strings.Replace(designDoc, `"summary": "Albums of photos."`, `"summary": "Albums of photos. The key is `+fakeKey+`."`, 1)

// A secret in a stage's output stops its publish, and nothing is published. Approving runs the stage again, with the
// finding (its rule and place, never its text) as its rejection; the new output is kept, scanned again and published.
func TestASecretInAnOutputRunsItsStageAgainAndNothingIsPublishedWhileItStands(t *testing.T) {
	script := "steps:\n  design:\n    - result: { status: ok, summary: wrote the design, output: '" + leakyDesign + "' }\n" +
		"    - result: { status: ok, summary: wrote the design again, output: '" + designDoc + "' }\n"
	r, fake := startSandboxed(t, publishNodes, script)
	onDay(r)
	keyScanner(t, fake)
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
	gf, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "design-docs-1.md"))
	for _, want := range []string{"Nothing was published, and nothing is while a finding stands.", "Approving runs design again",
		"as its rejection", "Odin keeps it, publishes it and scans it again", "The stages before design are kept."} {
		if !strings.Contains(string(gf), want) {
			t.Errorf("the gate file lacks %q:\n%s", want, gf)
		}
	}
	said := schema.Decisions{Decisions: []schema.Decision{{ID: "no-keys", Text: "Name the key's variable; never write a key."}}}
	if err := Approve(bg, r, "design-docs", said); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after approval = %+v, %v; want design run again and its new output published", out, err)
	}
	if len(r.State.Decisions) != 0 {
		t.Errorf("what a person gave at the secret stop is kept as a gate's decisions: %+v", r.State.Decisions)
	}
	// The stage ran again, told where the scan found what and by which rule, never the secret itself.
	prompt := promptOf(t, r, "design-2")
	rejection, _, _ := strings.Cut(prompt[strings.Index(prompt, "## Input: rejection"):], "\n## Your answer")
	for _, want := range []string{"## Input: rejection\n\nA person sent the last attempt back", "secret scan", "aws-access-token at " + designJSON + ":1", designMD,
		"before it is published.\n\nWhat the person said:\n\n- no-keys: Name the key's variable; never write a key.\n"} {
		if !strings.Contains(rejection, want) {
			t.Errorf("the rejection lacks %q:\n%s", want, rejection)
		}
	}
	if strings.Contains(prompt, fakeKey) {
		t.Error("the rejection holds the secret's text")
	}
	if got := string(readProject(t, r, designJSON)); got != designDoc {
		t.Errorf("published %s = %s; want the new output", designJSON, got)
	}
	if len(fake.ScanFiles) != 2 {
		t.Errorf("%d scans; the new output is scanned before it is published", len(fake.ScanFiles))
	}
	if r.State.Attempts["design"] != 2 {
		t.Errorf("design ran %d times, want 2", r.State.Attempts["design"])
	}
}

// An output that still holds the secret stops the publish again: nothing is ever published while a finding stands.
func TestASecretThatIsStillThereStopsThePublishAgain(t *testing.T) {
	script := "steps:\n  design:\n    - result: { status: ok, summary: wrote the design, output: '" + leakyDesign + "' }\n" +
		"    - result: { status: ok, summary: wrote it again, output: '" + leakyDesign + "' }\n"
	r, fake := startSandboxed(t, publishNodes, script)
	onDay(r)
	keyScanner(t, fake)
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	if out, err := Advance(bg, r); err != nil || out.Gate != "design-docs" {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if err := Approve(bg, r, "design-docs", Note(r, "design-docs", "")); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateRealSecrets || out.Gate != "design-docs" {
		t.Fatalf("Advance() = %+v, %v; want a second stop at the publish", out, err)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after != head {
		t.Error("a design with a secret was committed")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "gates", "design-docs-2.md")); err != nil {
		t.Errorf("the second stop wrote no gate file: %v", err)
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

// Ruling R50: a re-run from a hard stop (a blocked output, a secret) keeps the decisions of the gates between its
// source and the stop. The spec gate stops on a deviation and is approved with dark-mode; the spec's publish then
// stops; approving it runs the spec again, whose prompt holds dark-mode, and the owner check still requires it.
func TestAReRunFromAHardStopKeepsTheDecisionsOfTheGatesBetween(t *testing.T) {
	darkMode := schema.Decisions{Decisions: []schema.Decision{{ID: "dark-mode", Text: "No dark mode."}}}
	nodes := "  - { id: spec, kind: agent, role: planner, prompt: p.md, scratch: true, input: [decisions, rejection, refusal], output: ygg.spec/v1 }\n" +
		"  - { id: conformance, kind: agent, role: planner, prompt: p.md, scratch: true, reviews: spec, input: [spec], output: ygg.conformance/v1 }\n" +
		"  - { id: spec-gate, kind: gate, gate: spec, reruns: spec, deviation_from: conformance }\n" +
		"  - { id: spec-docs, kind: publish, from: spec, to: docs }\n"
	leaky := strings.Replace(specDoc, `"summary": "An API and a web app."`, `"summary": "An API. The key is `+fakeKey+`."`, 1)
	for name, c := range map[string]struct {
		first string
		gate  gungnir.Gate
	}{
		"blocked": {`{"blocked": {"reason": "The brief names no way to sign in."}}`, gungnir.GateExternalInput},
		"secret":  {leaky, gungnir.GateRealSecrets},
	} {
		t.Run(name, func(t *testing.T) {
			script := "steps:\n  spec:\n" +
				"    - result: { status: ok, summary: wrote the spec, output: '" + c.first + "' }\n" +
				"    - result: { status: ok, summary: wrote it again, output: '" + specDoc + "' }\n" +
				"    - result: { status: ok, summary: cited it, output: '" + specCiting("dark-mode", "dark-mode") + "' }\n" +
				"  conformance:\n    - result: { status: ok, summary: checked, output: '" + deviation + "' }\n" +
				"    - result: { status: ok, summary: checked, output: '" + noDeviation + "' }\n"
			r, fake := startSandboxed(t, nodes, script)
			onDay(r)
			keyScanner(t, fake)
			advanceTo(t, r, "spec-gate")
			if err := Approve(bg, r, "spec-gate", darkMode); err != nil {
				t.Fatal(err)
			}
			out, err := Advance(bg, r)
			if err != nil || out.Gate != "spec-docs" || r.State.PendingGate != c.gate {
				t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at spec-docs (%s)", out, err, r.State.PendingGate, c.gate)
			}
			if err := Approve(bg, r, "spec-docs", Note(r, "spec-docs", "")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.State.Decisions, []run.GateDecisions{{Gate: "spec-gate", Decisions: darkMode}}) {
				t.Errorf("decisions after the stop's approval = %+v; want spec-gate's dark-mode kept", r.State.Decisions)
			}
			if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
				t.Fatalf("Advance() after the approval = %+v, %v", out, err)
			}
			if p := promptOf(t, r, "spec-2"); !strings.Contains(p, "- dark-mode (at spec-gate): No dark mode.") {
				t.Errorf("the re-run spec's prompt lacks the kept decision:\n%s", p)
			}
			if p := promptOf(t, r, "spec-3"); !strings.Contains(p, "the spec does not cite the product owner's decisions dark-mode") {
				t.Errorf("the owner check did not require the kept decision:\n%s", p)
			}
			if r.State.Attempts["spec"] != 3 || !reflect.DeepEqual(r.State.Decisions, []run.GateDecisions{{Gate: "spec-gate", Decisions: darkMode}}) {
				t.Errorf("spec ran %d times, decisions %+v; want 3, and dark-mode kept", r.State.Attempts["spec"], r.State.Decisions)
			}
		})
	}
}
