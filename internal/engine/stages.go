package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// FeaturesFile is where Odin writes the plan's tasks and where each stands, in the project (spec §3.3 rule 2).
const FeaturesFile = ".factory/features.json"

// runTasks reads the plan's tasks.md, checks every task is ready (the Definition of Ready, spec §4.2) and
// derives features.json from it. A plan that isn't ready is a refusal: it goes back to the node n names
// with back_to, the planner, as input with every problem listed; with no back_to, or once the returns are
// used, the node fails. The tasks of a ready plan are recorded in build order, in run.json and in
// features.json, with all of them to do.
func runTasks(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	fs, source, sum, refusal, err := readPlan(r, n)
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
	s := r.State
	s.Features, s.PlanSource, s.PlanSHA256 = nil, source, sum
	for _, f := range fs {
		s.Features = append(s.Features, schema.FeatureRecord{Feature: f, Status: schema.TaskTodo})
	}
	if err := writeFeatures(r); err != nil {
		return err
	}
	s.Summaries[n.ID] = fmt.Sprintf("%d task(s) are ready, from %s", len(fs), source)
	return r.Log(gungnir.Event{Name: gungnir.EventTasksReady, Node: n.ID, Attempt: attempt, Result: s.Summaries[n.ID]})
}

// readPlan finds the one tasks.md that n.From names, reads it, and applies the Definition of Ready. A plan
// that can't be read or isn't ready gives a refusal, in words for the planner; err is Odin's own failure.
func readPlan(r *run.Run, n schema.Node) (fs []schema.Feature, source, sum, refusal string, err error) {
	matches, err := filepath.Glob(filepath.Join(r.Project, filepath.FromSlash(n.From)))
	if err != nil {
		return nil, "", "", fmt.Sprintf("tasks: %q is not a path pattern: %v", n.From, err), nil
	}
	var files []string
	for _, m := range matches {
		if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() {
			files = append(files, m)
		}
	}
	switch {
	case len(files) == 0:
		return nil, "", "", fmt.Sprintf("there is no tasks file at %s, so there is no plan to build. Write the plan's tasks.md there.", n.From), nil
	case len(files) > 1:
		return nil, "", "", fmt.Sprintf("%d files match %s, and a plan has exactly one tasks.md: keep one.", len(files), n.From), nil
	}
	f, err := os.Open(files[0])
	if err != nil {
		return nil, "", "", "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, schema.MaxTasksFile+1))
	if err != nil {
		return nil, "", "", "", err
	}
	source, err = filepath.Rel(r.Project, files[0])
	if err != nil {
		return nil, "", "", "", err
	}
	source = filepath.ToSlash(source)
	if fs, err = schema.ParseTasks(source, data); err != nil {
		return nil, "", "", err.Error(), nil
	}
	leaves, refusal, err := planLeaves(r, n)
	if err != nil || refusal != "" {
		return nil, "", "", refusal, err
	}
	if err := schema.CheckReady(fs, leaves); err != nil {
		return nil, "", "", err.Error(), nil
	}
	if fs, err = schema.ReadyOrder(fs); err != nil {
		return nil, "", "", err.Error(), nil
	}
	h := sha256.Sum256(data)
	return fs, source, hex.EncodeToString(h[:]), "", nil
}

// planLeaves returns the leaves a task may work in: those the project has, and those the spec stage named
// in the leaves file the node points at, which init has not created yet.
func planLeaves(r *run.Run, n schema.Node) (leaves []string, refusal string, err error) {
	if u := r.Setup.Project.Umbrella; u != nil {
		for _, p := range u.Paths() {
			if u.Children[p].Kind == schema.ChildLeaf {
				leaves = append(leaves, p)
			}
		}
	}
	if n.Leaves != "" {
		file := filepath.Join(r.Project, filepath.FromSlash(n.Leaves))
		data, rerr := os.ReadFile(file)
		if rerr != nil {
			return nil, fmt.Sprintf("the leaves file %s can't be read (%v), so the tasks' leaves can't be checked. The spec stage names the product's leaves there.", n.Leaves, rerr), nil
		}
		l, perr := schema.ParseLeaves(n.Leaves, data)
		if perr != nil {
			return nil, perr.Error(), nil
		}
		leaves = append(leaves, l.Paths()...)
	}
	slices.Sort(leaves)
	return slices.Compact(leaves), "", nil
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
