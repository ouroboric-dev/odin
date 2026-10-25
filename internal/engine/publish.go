package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ouroboric-dev/gleipnir/scan"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/views"
	"go.yaml.in/yaml/v3"
)

// runPublish publishes the typed output of the scratch agent n.From into the project, under the folder the node
// names, and commits it in the project umbrella (spec §3.3 rule 1: "Odin commits the planner's output into L1
// after the spec approval point"). What it publishes is the output's JSON, the bytes Odin kept and validated, and
// its view, which Odin renders from it (change typed-boundaries, design §2: Markdown is a view); for a spec, also
// the leaves file, which Odin writes from the spec's typed leaves. Nothing of the node's out/ is published: out/
// holds the session's drafts. The documents are scanned for secrets first, when the run has a sandbox to scan in.
// Odin commits with host git, by explicit paths.
func runPublish(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	// A blocked output says what only a person can give: the run stops for one, before anything is scanned or
	// published, and approving runs the source again with the person's note as its rejection (ruling R43).
	switch b, err := blockedOf(r, n.From); {
	case err != nil:
		return &nodeFailure{fmt.Errorf("%s publishes the output of %s, and %w", n.ID, n.From, err)}
	case b != nil:
		return &hardStop{gate: gungnir.GateExternalInput, err: fmt.Errorf("%s is blocked: %s", n.From, b.Reason)}
	}
	docs, view, err := publication(r, n)
	if err != nil {
		return &nodeFailure{fmt.Errorf("%s publishes the output of %s, and %w", n.ID, n.From, err)}
	}
	if err := scanDocuments(ctx, r, n, docs); err != nil {
		return err
	}
	project := r.Project
	if staged, err := git.Run(ctx, project, "diff", "--cached", "--name-only"); err != nil {
		return err
	} else if staged != "" {
		return &nodeFailure{fmt.Errorf("the project has staged changes Odin didn't make (%s); commit or unstage them first", strings.ReplaceAll(staged, "\n", ", "))}
	}
	var paths []string
	for _, d := range docs {
		if err := copyDocument(project, d.data, filepath.Join(project, filepath.FromSlash(d.path))); err != nil {
			return &nodeFailure{fmt.Errorf("publishing %s: %w", d.path, err)}
		}
		paths = append(paths, d.path)
	}
	ev := gungnir.Event{Name: gungnir.EventDocsPublished, Node: n.ID, Attempt: attempt}
	if _, err := git.Run(ctx, project, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	if changed, err := git.Run(ctx, project, "diff", "--cached", "--name-only"); err != nil {
		return err
	} else if changed != "" {
		msg := fmt.Sprintf("Publish %s's documents (run %s, node %s)", n.From, r.State.ID, n.ID)
		if _, err := git.Run(ctx, project, append([]string{"commit", "-q", "-m", msg, "--"}, paths...)...); err != nil {
			return err
		}
		if ev.Commit, err = git.Run(ctx, project, "rev-parse", "HEAD"); err != nil {
			return err
		}
	}
	if ev.Commit == "" {
		ev.Note = fmt.Sprintf("%d file(s) under %s, unchanged", len(paths), n.To)
	} else {
		ev.Note = fmt.Sprintf("%d file(s) under %s: %s", len(paths), n.To, strings.Join(paths, ", "))
	}
	ref := r.State.Outputs[n.From]
	ref.View = view
	r.State.Outputs[n.From] = ref
	r.State.Summaries[n.ID] = ev.Note
	return r.Log(ev)
}

// document is one file a publish node writes: its path in the project, as a slash path, and its bytes.
type document struct {
	path string
	data []byte
}

// publication is what node n publishes of the output of n.From, at the paths the documents had when the agents
// wrote them, under n.To, with the run's date: the JSON and the view, and view, the view's path (a plan's is
// plan.md). runPublish has stopped at a blocked output before it gets here.
func publication(r *run.Run, n schema.Node) ([]document, string, error) {
	c := schema.Contract(r.State.Outputs[n.From].Contract)
	date := dateOf(r.Setup)
	switch c {
	case schema.ContractDesign:
		d, raw, err := loadOutputBytes[schema.Design](r, n.From)
		if err != nil {
			return nil, "", err
		}
		base := path.Join(n.To, "1-product", "design", date+"-"+d.Slug)
		md, err := views.Render(c, d, front(r, n, d.Title+" design", date, base+".md"))
		return []document{{base + ".json", raw}, {base + ".md", md}}, base + ".md", err
	case schema.ContractSpec:
		sp, raw, err := loadOutputBytes[schema.Spec](r, n.From)
		if err != nil {
			return nil, "", err
		}
		base := path.Join(n.To, "2-engineering", "specs", date+"-"+sp.Slug)
		md, err := views.Render(c, sp, front(r, n, sp.Title+" spec", date, base+".md"))
		if err != nil {
			return nil, "", err
		}
		leaves, err := leavesFile(sp.Leaves, base+".json")
		return []document{{base + ".json", raw}, {base + ".md", md}, {path.Join(n.To, "leaves.yaml"), leaves}}, base + ".md", err
	case schema.ContractPlan:
		p, raw, err := loadOutputBytes[schema.Plan](r, n.From)
		if err != nil {
			return nil, "", err
		}
		dir := path.Join(n.To, "2-engineering", "plans", date+"-"+p.Slug)
		md, err := views.Render(c, p, front(r, n, p.Title+" plan", date, path.Join(dir, "plan.md")))
		if err != nil {
			return nil, "", err
		}
		tasks, err := views.Tasks(p, front(r, n, p.Title+" tasks", date, path.Join(dir, "tasks.md")))
		return []document{{path.Join(dir, "plan.json"), raw}, {path.Join(dir, "plan.md"), md}, {path.Join(dir, "tasks.md"), tasks}}, path.Join(dir, "plan.md"), err
	case schema.ContractReuseReview:
		rv, raw, err := loadOutputBytes[schema.ReuseReview](r, n.From)
		if err != nil {
			return nil, "", err
		}
		// A reuse review is named for the plan it reviews, and sits beside the other reviews.
		title, slug, err := reviewed(r, n.From)
		if err != nil {
			return nil, "", err
		}
		base := path.Join(n.To, "2-engineering", "reviews", date+"-"+slug+"-reuse-review")
		md, err := views.Render(c, rv, front(r, n, title+" reuse review", date, base+".md"))
		return []document{{base + ".json", raw}, {base + ".md", md}}, base + ".md", err
	}
	// The graph check refuses a publish node whose source has no view, when the run starts.
	return nil, "", fmt.Errorf("its output, a %s, has no view", c)
}

// blockedOf is the blocked field of the output node kept, nil when the output is not blocked: every agent output
// may carry one (change typed-boundaries, design §1). It reads the kept bytes, as Odin validated them.
func blockedOf(r *run.Run, node string) (*gungnir.Blocked, error) {
	_, raw, err := loadOutputBytes[json.RawMessage](r, node)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Blocked *gungnir.Blocked `json:"blocked"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Blocked, nil
}

// reviewed is the title and the slug of the output that node reviews: the plan a reuse review is of.
func reviewed(r *run.Run, node string) (title, slug string, err error) {
	nodes := r.Nodes()
	i := indexOf(nodes, node)
	if i < 0 || nodes[i].Reviews == "" {
		return "", "", fmt.Errorf("%s reviews no node, and its review is named for the plan it reviews", node)
	}
	of := nodes[i].Reviews
	switch c := schema.Contract(r.State.Outputs[of].Contract); c {
	case schema.ContractPlan:
		p, err := loadOutput[schema.Plan](r, of)
		return p.Title, p.Slug, err
	case schema.ContractSpec:
		sp, err := loadOutput[schema.Spec](r, of)
		return sp.Title, sp.Slug, err
	case schema.ContractDesign:
		d, err := loadOutput[schema.Design](r, of)
		return d.Title, d.Slug, err
	default:
		return "", "", fmt.Errorf("%s reviews %s, whose output (%s) has no title or slug to name its review for", node, of, c)
	}
}

// front is the front matter of the view of n.From, at view: its title, the run's date, and links to the views
// already published of the outputs the node read, relative to the view.
func front(r *run.Run, n schema.Node, title, date, view string) views.Front {
	related := []string{}
	nodes := r.Nodes()
	if i := indexOf(nodes, n.From); i >= 0 {
		for _, in := range sessionInputs(nodes[i], nil) {
			if ref, ok := r.State.Outputs[in]; ok && ref.View != "" {
				if rel, err := filepath.Rel(path.Dir(view), ref.View); err == nil {
					related = append(related, filepath.ToSlash(rel))
				}
			}
		}
	}
	return views.Front{Title: title, Date: date, Related: related}
}

// leavesFile is leaves.yaml, written from the spec's typed leaves: the tasks and init nodes read it, and the
// project keeps it as it kept the file the spec stage wrote before its output was typed.
func leavesFile(l schema.Leaves, from string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Written by Odin from the leaves of the spec, " + path.Base(from) + ".\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(l); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// publishStage is the folder of the source's scratch workspace where Odin writes the documents it will publish,
// for the scan: the bytes it will commit, not anything an agent can change after.
const publishStage = ".publish"

// scanDocuments looks for secrets in the documents, in the scratch sandbox of the node whose output they are,
// where gitleaks is pinned (spec §14). A run with no sandbox has nowhere to run the scanner, and only scripted
// agents. Secrets are a hard stop at real-secrets-or-user-data with nothing published; a scan that could not run
// fails the node, because no answer is never "clean". Odin writes the documents under publishStage in the
// workspace, at their paths in the project, and runs the scan there, so its report names them as the project will.
func scanDocuments(ctx context.Context, r *run.Run, n schema.Node, docs []document) error {
	runner, err := r.ScratchRunner(ctx, n.From)
	if err != nil {
		return sandboxStart(err)
	}
	if runner == nil || len(docs) == 0 {
		return nil
	}
	listed := make([]string, len(docs))
	data := make([][]byte, len(docs))
	for i, d := range docs {
		listed[i], data[i] = d.path, d.data
	}
	if err := stageDocuments(filepath.Join(r.ScratchDir(n.From), publishStage), listed, data); err != nil {
		return err
	}
	err = scan.Run(ctx, inDir{runner, publishStage}, listed)
	var found *scan.FoundError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &found):
		// Approving this stop runs n.From again, with the findings as its rejection (Approve).
		r.State.Reasons[n.From] = secretRejection(found)
		return &hardStop{gate: gungnir.GateRealSecrets, err: err}
	}
	return &nodeFailure{err}
}

// secretRejection is what the node whose output the scan stopped is told when it runs again: each finding's rule
// and place, in the documents Odin rendered from its output, safe for a prompt as it is (the rejection input). A
// finding never holds the secret's text.
func secretRejection(found *scan.FoundError) string {
	places := make([]string, len(found.Findings))
	for i, f := range found.Findings {
		places[i] = fmt.Sprintf("- %s at %s:%d", mdsafe.Line(f.Rule), mdsafe.Line(f.File), f.Line)
	}
	return "Odin's secret scan found what looks like a secret in the documents of your last output, so nothing was published:\n\n" +
		strings.Join(places, "\n") + "\n\nWrite your output again without it. The new output is scanned again before it is published."
}

// stageDocuments makes stage anew and writes each document's bytes in it, at its path, as a new file.
func stageDocuments(stage string, files []string, docs [][]byte) error {
	if err := emptyDir(stage); err != nil {
		return err
	}
	for i, f := range files {
		p := filepath.Join(stage, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return err
		}
		_, err = w.Write(docs[i])
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// inDir runs every command in dir, a folder of the runner's workspace.
type inDir struct {
	gungnir.Runner
	dir string
}

func (d inDir) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	c.Dir = path.Join(d.dir, c.Dir)
	return d.Runner.Run(ctx, c)
}

// noSymlinks refuses a path under the project if any part of it that exists, from the project down, is a
// symlink: a write or a read through one would leave the project. Parts that don't exist yet are fine.
func noSymlinks(project, path string) error {
	rel, err := filepath.Rel(project, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s is not inside the project", path)
	}
	cur := project
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", cur)
		}
	}
	return nil
}

// copyDocument writes data, a document as publication made it, to dst, making dst's folders. Nothing on
// the way to dst may be a symlink: a document must land where the node says, inside the project.
func copyDocument(project string, data []byte, dst string) error {
	if err := noSymlinks(project, dst); err != nil {
		return err
	}
	if st, err := os.Lstat(dst); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
