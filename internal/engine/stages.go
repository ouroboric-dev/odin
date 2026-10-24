package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
)

// FeaturesFile is where Odin writes the plan's tasks and where each stands, in the project (spec §3.3 rule 2).
const FeaturesFile = ".factory/features.json"

// runTasks reads the plan, the typed output of the node n names with from (ygg.plan/v1), checks every task is ready
// (the Definition of Ready, spec §4.2) and derives features.json from it. A plan that isn't ready is a refusal: it
// goes back to the node n names with back_to, the planner, as input with every problem listed; with no back_to, or
// once the returns are used, the node fails. A blocked plan says what only a person can give, so it stops the run
// for one, in every mode, and is not sent back. The tasks of a ready plan are recorded in build order, in run.json
// and in features.json, with all of them to do. A node that names a leaves file also checks that the project's
// runtime realm provides every need of the file's realms (spec §6.1): init runs after it, and the plan can't change
// the leaves file, so an unmet need fails the node and goes back to no one.
func runTasks(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	fs, source, sum, spec, refusal, err := readPlan(r, n)
	if err != nil {
		return err
	}
	if refusal != "" {
		ev := gungnir.Event{Name: gungnir.EventNodeFailed, Node: n.ID, Attempt: attempt, Result: firstLine(refusal)}
		if err := r.Log(ev); err != nil {
			return err
		}
		if sent, err := sendBack(r, n, refusal); sent || err != nil {
			return err
		}
		return &nodeFailure{errors.New(refusal)}
	}
	if err := checkLeavesNeeds(r, spec); err != nil {
		if lerr := r.Log(gungnir.Event{Name: gungnir.EventNodeFailed, Node: n.ID, Attempt: attempt, Result: firstLine(err.Error())}); lerr != nil {
			return lerr
		}
		return &nodeFailure{err}
	}
	s := r.State
	s.Features, s.PlanSource, s.PlanSHA256 = nil, source, sum
	for _, f := range fs {
		s.Features = append(s.Features, schema.FeatureRecord{Feature: f, Status: schema.TaskTodo})
	}
	if err := writeFeatures(r); err != nil {
		return err
	}
	s.Summaries[n.ID] = fmt.Sprintf("%d task(s) are ready, from the output of %s (%s)", len(fs), n.From, source)
	return r.Log(gungnir.Event{Name: gungnir.EventTasksReady, Node: n.ID, Attempt: attempt, Result: s.Summaries[n.ID]})
}

// readPlan reads the plan node n.From kept, as Odin validated it, and applies the Definition of Ready to its tasks.
// Nothing reads a tasks.md: Odin renders that file from this output, for people. A plan that isn't ready gives a
// refusal, in words for the planner; a blocked plan is a *hardStop at external-input, with the planner's reason; err
// is otherwise Odin's own failure. source is the kept output's path in the project and sum its SHA-256; spec is the
// leaves file the node points at, parsed, and nil when it points at none.
func readPlan(r *run.Run, n schema.Node) (fs []schema.Feature, source, sum string, spec *schema.Leaves, refusal string, err error) {
	p, err := loadOutput[schema.Plan](r, n.From)
	if err != nil {
		return nil, "", "", nil, "", &nodeFailure{fmt.Errorf("the plan can't be read: %w", err)}
	}
	if p.Blocked != nil {
		return nil, "", "", nil, "", &hardStop{gate: gungnir.GateExternalInput, err: fmt.Errorf("the plan is blocked: %s", p.Blocked.Reason)}
	}
	ref := r.State.Outputs[n.From]
	if source, err = filepath.Rel(r.Project, filepath.Join(r.Dir, filepath.FromSlash(ref.Path))); err != nil {
		return nil, "", "", nil, "", err
	}
	fs = schema.FeaturesFromPlan(p)
	leaves, spec, refusal, err := planLeaves(r, n)
	if err != nil || refusal != "" {
		return nil, "", "", nil, refusal, err
	}
	if err := schema.CheckReady(fs, leaves); err != nil {
		return nil, "", "", nil, err.Error(), nil
	}
	if fs, err = schema.ReadyOrder(fs); err != nil {
		return nil, "", "", nil, err.Error(), nil
	}
	return fs, filepath.ToSlash(source), ref.SHA256, spec, "", nil
}

// checkLeavesNeeds checks the needs of the realms of the leaves file spec (none when spec is nil), with the
// runtime realm the file pins or the project records. The realms are loaded from the realm store as init loads
// them, and nothing is recorded: init, after this node, takes them. Realms that can't be loaded, or pins that
// disagree, are not this node's to refuse: init refuses them, with every problem, before it creates anything.
func checkLeavesNeeds(r *run.Run, spec *schema.Leaves) error {
	if spec == nil {
		return nil
	}
	_, runtime, leafRealms, err := planLeafRealms(r, spec)
	if err != nil {
		return nil // init's refusal, which it makes before it creates anything
	}
	return r.Setup.CheckNeedsWith(runtime, leafRealms)
}

// planLeaves returns the leaves a task may work in: those the project has, and those the spec stage named
// in the leaves file the node points at, which init has not created yet. spec is that file, parsed: nil when
// the node points at none.
func planLeaves(r *run.Run, n schema.Node) (leaves []string, spec *schema.Leaves, refusal string, err error) {
	leaves = []string{} // never nil: no leaf at all is a list a layer is checked against, not no check
	if u := r.Setup.Project.Umbrella; u != nil {
		for _, p := range u.Paths() {
			if u.Children[p].Kind == schema.ChildLeaf {
				leaves = append(leaves, p)
			}
		}
	}
	if n.Leaves != "" {
		// The leaves file is the one Odin wrote from the spec's typed leaves when it published the spec (spec-docs),
		// read back as data Odin wrote, not as anything an agent wrote.
		data, rerr := readLeavesFile(r, n.Leaves)
		if rerr != nil {
			return nil, nil, fmt.Sprintf("the leaves file %s can't be read (%v), so the tasks' leaves can't be checked. The spec stage names the product's leaves there.", n.Leaves, rerr), nil
		}
		l, perr := schema.ParseLeaves(n.Leaves, data)
		if perr != nil {
			return nil, nil, perr.Error(), nil
		}
		leaves = append(leaves, l.Paths()...)
		spec = l
	}
	slices.Sort(leaves)
	return slices.Compact(leaves), spec, "", nil
}

// writeFeatures writes features.json from the run's record of the tasks.
func writeFeatures(r *run.Run) error {
	s := r.State
	if err := os.MkdirAll(filepath.Join(r.Project, ".factory"), 0o755); err != nil {
		return err
	}
	f := &schema.Features{Run: s.ID, Source: s.PlanSource, SourceSHA256: s.PlanSHA256, Tasks: slices.Clone(s.Features)}
	return f.Save(filepath.Join(r.Project, filepath.FromSlash(FeaturesFile)))
}

func clip(s string, n int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > n {
		return string(r[:n]) + "…"
	}
	return strings.TrimSpace(s)
}

// taskInput is the section of the task input: what the session's task is, as the plan states it (spec §4.4: a
// session reads its task from features.json, and Odin, the only reader of that file, puts the task in the prompt).
// It is empty for a node that works for no task. In order: the title, the layer, the criteria, the named test and
// the dependencies, then the planner's text under "What the plan says" and, when the task has one, its OpenAPI text
// under "The contract", in a yaml fence no run of backticks in it can close. Every field is whole: the plan's schema
// and the readiness check bound them, and Odin never cuts one (B-176). Every line is data to the session, written so
// that none starts a heading.
func taskInput(r *run.Run, n schema.Node) string {
	i := slices.IndexFunc(r.State.Features, func(f schema.FeatureRecord) bool { return n.Task != "" && f.Slug() == n.Task })
	if i < 0 {
		return ""
	}
	f := r.State.Features[i]
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\nAcceptance criteria:\n", mdsafe.Text(f.ID+": "+mdsafe.Line(f.Title)), mdsafe.Text("You work in the leaf "+f.Layer+"."))
	b.WriteString(mdsafe.Bullets(f.Criteria))
	fmt.Fprintf(&b, "\n%s\n", mdsafe.Text("The test that proves it: "+mdsafe.Line(f.Test)))
	if len(f.DependsOn) > 0 {
		fmt.Fprintf(&b, "\n%s\n", mdsafe.Text("It builds on: "+strings.Join(f.DependsOn, ", ")+" (already landed)."))
	}
	if strings.TrimSpace(f.Text) != "" {
		b.WriteString("\n### What the plan says\n\n" + mdsafe.Text(f.Text) + "\n")
	}
	if strings.TrimSpace(f.Contract) != "" {
		b.WriteString("\n### The contract\n\n" + mdsafe.Fenced("yaml", f.Contract))
	}
	return b.String()
}

// expanded is a foreach node that became the nodes of its tasks: Position now points at the first of them.
type expanded struct{}

func (*expanded) Error() string { return "expanded" }

// expandForeach turns foreach node n into its task shape once for each task the plan holds, in the leaf and
// the realm each task names (spec §4.2, change realm-leaf-naming). A plan the tasks node has not read, or a
// task whose leaf the project lacks, fails the node.
func expandForeach(ctx context.Context, r *run.Run, n schema.Node) error {
	if len(r.State.Features) == 0 {
		return &nodeFailure{fmt.Errorf("%s repeats its nodes for every task, and the run has no plan yet: a tasks node must come before it", n.ID)}
	}
	if err := r.Expand(ctx, n.ID); err != nil {
		return &nodeFailure{err}
	}
	return &expanded{}
}

// finishTask marks done the task whose last node the walk has just left, and startTask marks doing the
// task whose first node is next. Each writes features.json and a line in the ledger when a task changes.
// A task that is skipped because one it depends on is blocked is neither: it is blocked, by skipBlocked,
// which runs between the two.
func finishTask(r *run.Run, nodes []schema.Node) error {
	s := r.State
	if s.Position > 0 && s.Position <= len(nodes) {
		if prev := nodes[s.Position-1]; prev.Task != "" && (s.Position == len(nodes) || nodes[s.Position].Task != prev.Task) {
			return setTask(r, prev.Task, schema.TaskDone, gungnir.EventTaskDone)
		}
	}
	return nil
}

func startTask(ctx context.Context, r *run.Run, nodes []schema.Node) error {
	s := r.State
	if s.Position >= len(nodes) || nodes[s.Position].Task == "" {
		return nil
	}
	n := nodes[s.Position]
	i := slices.IndexFunc(s.Features, func(f schema.FeatureRecord) bool { return f.Slug() == n.Task })
	if rec, ok := s.Leaves[n.Repo]; ok && n.Repo != "" && i >= 0 && s.Features[i].Status == schema.TaskTodo {
		// Where the leaf's main is as the task starts, saved with the task's start: blockTask puts main back
		// there if the task is given up after its merge landed.
		l, err := r.Leaf(n.Repo)
		if err != nil {
			return err
		}
		if rec.TaskBase, err = l.Rev(ctx, "main"); err != nil {
			return err
		}
		s.Leaves[n.Repo] = rec
	}
	return setTask(r, n.Task, schema.TaskDoing, gungnir.EventTaskStarted)
}

// setTask moves the task with the node id slug to status, if that moves it forward: todo to doing, and
// todo or doing to done. A task that is already done, or blocked, stays as it is.
func setTask(r *run.Run, slug string, to schema.TaskStatus, event gungnir.EventName) error {
	s := r.State
	i := slices.IndexFunc(s.Features, func(f schema.FeatureRecord) bool { return f.Slug() == slug })
	if i < 0 {
		return nil
	}
	from := s.Features[i].Status
	forward := (to == schema.TaskDoing && from == schema.TaskTodo) || (to == schema.TaskDone && (from == schema.TaskTodo || from == schema.TaskDoing))
	if !forward {
		return nil
	}
	s.Features[i].Status = to
	if err := writeFeatures(r); err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: event, Task: slug, Note: s.Features[i].ID}); err != nil {
		return err
	}
	return r.Save()
}

// blocksTasks reports whether the run gives a task up when its loop does, and goes on with the rest:
// an unattended run's answer to an exhausted loop, where the other modes stop and ask (spec §7.3).
func blocksTasks(r *run.Run, n schema.Node) bool {
	return r.State.Mode == gungnir.ModeUnattended && n.Task != ""
}

// blockTask gives up the task node n works for: the task is blocked, with the reason, the leaf is put
// back to its main as it was when the task started, with what the task did kept on a ref for the
// operator, and Position moves past the task's nodes. A merge of the task that landed is taken off main
// too, so no later task builds on work a gate refused. The tasks that depend on it are blocked when the
// walk reaches them (skipBlocked).
func blockTask(ctx context.Context, r *run.Run, nodes []schema.Node, n schema.Node, why string) error {
	s := r.State
	if rec, ok := s.Leaves[n.Repo]; ok && n.Repo != "" {
		l, err := r.Leaf(n.Repo)
		if err != nil {
			return err
		}
		ref := "refs/ygg/" + s.ID + "/blocked/" + n.Task
		if _, err := l.Git(ctx, "update-ref", ref, rec.Checkpoint); err != nil {
			return err
		}
		base, err := mainBack(ctx, r, l, n.Repo, rec.TaskBase)
		if err != nil {
			return err
		}
		if err := l.Reset(ctx, rec.Branch, base); err != nil {
			return err
		}
		rec.Checkpoint, rec.TaskBase = base, ""
		s.Leaves[n.Repo] = rec
		why += fmt.Sprintf(" What the task did is kept on %s (git -C %s log %s).", ref, r.RepoDir(n.Repo), ref)
	}
	if err := markBlocked(r, n.Task, why); err != nil {
		return err
	}
	for s.Position < len(nodes) && nodes[s.Position].Task == n.Task {
		s.Position++
	}
	s.Pending, s.PendingGate = "", ""
	return r.Save()
}

// mainBack moves the leaf's main back to taskBase, under the leaf's lock, and returns where main is. A
// main that has moved on since the task started holds only the task's merge, since tasks run one at a
// time; taskBase must be an ancestor of it all the same, or main stays where it is. An empty taskBase
// (a run.json from before it was recorded) leaves main as it is.
func mainBack(ctx context.Context, r *run.Run, l *leaf.Leaf, repo, taskBase string) (string, error) {
	release, err := lock.Take(r.LockFile(repo))
	if err != nil {
		return "", err
	}
	defer release()
	main, err := l.Rev(ctx, "main")
	if err != nil || taskBase == "" || taskBase == main {
		return main, err
	}
	if _, err := l.Git(ctx, "merge-base", "--is-ancestor", taskBase, main); err != nil {
		return "", fmt.Errorf("the leaf's main is at %s, which doesn't hold %s, where it was when the task started: %w", main, taskBase, err)
	}
	// update-ref with the old value moves main only if it is still where it was read.
	if _, err := l.Git(ctx, "update-ref", "refs/heads/main", taskBase, main); err != nil {
		return "", err
	}
	return taskBase, nil
}

// markBlocked records the task as blocked, in features.json and the ledger.
func markBlocked(r *run.Run, slug, why string) error {
	s := r.State
	i := slices.IndexFunc(s.Features, func(f schema.FeatureRecord) bool { return f.Slug() == slug })
	if i < 0 || s.Features[i].Status == schema.TaskBlocked {
		return nil
	}
	s.Features[i].Status, s.Features[i].Why = schema.TaskBlocked, why
	if err := writeFeatures(r); err != nil {
		return err
	}
	return r.Log(gungnir.Event{Name: gungnir.EventTaskBlocked, Task: slug, Result: "blocked", Note: firstLine(why)})
}

// skipBlocked blocks, without starting it, the task whose nodes come next if a task it depends on is
// blocked, and moves Position past the task's nodes. It returns whether it moved.
func skipBlocked(r *run.Run, nodes []schema.Node) (bool, error) {
	s := r.State
	moved := false
	for s.Position < len(nodes) && nodes[s.Position].Task != "" {
		slug := nodes[s.Position].Task
		i := slices.IndexFunc(s.Features, func(f schema.FeatureRecord) bool { return f.Slug() == slug })
		if i < 0 {
			return moved, nil
		}
		f := s.Features[i]
		if f.Status != schema.TaskBlocked {
			dep := slices.IndexFunc(s.Features, func(g schema.FeatureRecord) bool {
				return g.Status == schema.TaskBlocked && slices.Contains(f.DependsOn, g.ID)
			})
			if dep < 0 {
				return moved, nil
			}
			if err := markBlocked(r, slug, fmt.Sprintf("it depends on %s, which is blocked", s.Features[dep].ID)); err != nil {
				return moved, err
			}
		}
		for s.Position < len(nodes) && nodes[s.Position].Task == slug {
			s.Position++
		}
		moved = true
	}
	if moved {
		return true, r.Save()
	}
	return false, nil
}
