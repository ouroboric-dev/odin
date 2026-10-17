package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/snapshot"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// NewSandbox builds the sandbox driver of a run. dir is where it keeps its configuration, inside
// the run's own directory. Tests replace it with a fake.
type NewSandbox func(s *SandboxSetup, dir string) (gungnir.Sandbox, error)

// defaultSandbox is the devcontainer driver on the engine factory.yaml names.
func defaultSandbox(s *SandboxSetup, dir string) (gungnir.Sandbox, error) {
	return sandbox.New(s.Engine, s.Config, dir)
}

// openSandbox gives the run its driver, when its config has a sandbox section.
func (r *Run) openSandbox(open NewSandbox) error {
	if r.Setup.Sandbox == nil {
		return nil
	}
	if open == nil {
		open = defaultSandbox
	}
	var err error
	r.Sandbox, err = open(r.Setup.Sandbox, filepath.Join(r.Dir, "sandbox"))
	return err
}

// Runner returns what runs commands in repo's sandbox, starting the sandbox the first time. It
// returns nil when the run has no sandbox, or repo is an umbrella: Odin's own generators run on the
// host. A sandbox is started once per run and leaf, and Advance stops it again when it leaves.
func (r *Run) Runner(ctx context.Context, repo string) (gungnir.Runner, error) {
	if r.Sandbox == nil || !r.Setup.isLeaf(repo) {
		return nil, nil
	}
	if env, ok := r.State.Sandboxes[repo]; ok {
		return gungnir.Bind(r.Sandbox, env), nil
	}
	nodes, err := tree.Resolve(ctx, r.Setup.Root, filepath.Join(r.Setup.ProjectPath, repo))
	if err != nil {
		return nil, err
	}
	parent, leaf := nodes[len(nodes)-2], nodes[len(nodes)-1]
	stack, egress, services, err := r.Setup.SandboxFor(repo)
	if err != nil {
		return nil, err
	}
	env, err := r.Sandbox.Up(ctx, gungnir.SandboxSpec{Run: r.State.ID, Name: repo, Parent: parent.Dir, Leaf: leaf.Dir,
		GitDir: leaf.GitDir, Egress: egress, Stack: stack, Services: services})
	if err != nil {
		return nil, err
	}
	if r.State.Sandboxes == nil {
		r.State.Sandboxes = map[string]gungnir.Env{}
	}
	// Recorded before anything runs in it: a ygg that dies from here on leaves a container the next
	// resume, and doctor, can find and stop.
	r.State.Sandboxes[repo] = env
	if err := r.Log(gungnir.Event{Name: gungnir.EventSandboxUp, Note: fmt.Sprintf("%s: container %.12s", repo, env.Container)}); err != nil {
		return nil, err
	}
	if err := r.Save(); err != nil {
		return nil, err
	}
	return gungnir.Bind(r.Sandbox, env), nil
}

// ScratchDir is the workspace of a scratch agent node: a folder of the run, beside the leaves and apart
// from them (spec §3.3 rule 1).
func (r *Run) ScratchDir(node string) string { return filepath.Join(r.Dir, "scratch", node) }

// ScratchRunner returns what runs commands in the sandbox of node's scratch workspace, starting it the
// first time. It returns nil when the run has no sandbox: a scripted-only run works on the host. The
// sandbox holds the workspace and nothing else, on the lead realm's stack, and is recorded under the
// node's scratch name so that Advance, a resume and doctor stop it like a leaf's.
func (r *Run) ScratchRunner(ctx context.Context, node string) (gungnir.Runner, error) {
	if r.Sandbox == nil {
		return nil, nil
	}
	key := "scratch/" + node
	if env, ok := r.State.Sandboxes[key]; ok {
		return gungnir.Bind(r.Sandbox, env), nil
	}
	env, err := r.Sandbox.Up(ctx, gungnir.SandboxSpec{Run: r.State.ID, Name: key, Parent: r.Dir, Leaf: r.ScratchDir(node), Scratch: true,
		Egress: r.Setup.Sandbox.Egress, Stack: r.Setup.Lead.Stack})
	if err != nil {
		return nil, err
	}
	if r.State.Sandboxes == nil {
		r.State.Sandboxes = map[string]gungnir.Env{}
	}
	r.State.Sandboxes[key] = env
	if err := r.Log(gungnir.Event{Name: gungnir.EventSandboxUp, Note: fmt.Sprintf("%s: container %.12s", key, env.Container)}); err != nil {
		return nil, err
	}
	if err := r.Save(); err != nil {
		return nil, err
	}
	return gungnir.Bind(r.Sandbox, env), nil
}

// SnapshotRunner returns what runs commands in the sandbox that holds a snapshot of the project, for the
// acceptance checks of node: the committed files of the project umbrella and of each leaf, at the commits
// they are on, or at the commits revs names for a leaf (by its path), in a plain folder of the run. The
// snapshot is made again every time, since the work it holds changes between attempts; the sandbox
// is started the first time, with the services of every leaf's realm, and is recorded under its own name
// so that Advance, a resume and doctor stop it like a leaf's (change realm-content).
func (r *Run) SnapshotRunner(ctx context.Context, node string, revs map[string]string) (gungnir.Runner, error) {
	if r.Sandbox == nil {
		return nil, nil
	}
	key, dir := "snapshot/"+node, filepath.Join(r.Dir, "snapshot", node)
	sources, err := r.snapshotSources(ctx, revs)
	if err != nil {
		return nil, &SnapshotError{err}
	}
	if err := snapshot.Export(ctx, dir, sources); err != nil {
		return nil, &SnapshotError{err}
	}
	if env, ok := r.State.Sandboxes[key]; ok {
		return gungnir.Bind(r.Sandbox, env), nil
	}
	stack, egress, services, err := r.Setup.SnapshotSandbox()
	if err != nil {
		return nil, err
	}
	env, err := r.Sandbox.Up(ctx, gungnir.SandboxSpec{Run: r.State.ID, Name: key, Parent: r.Dir, Leaf: dir, Scratch: true,
		Egress: egress, Stack: stack, Services: services})
	if err != nil {
		return nil, err
	}
	if r.State.Sandboxes == nil {
		r.State.Sandboxes = map[string]gungnir.Env{}
	}
	r.State.Sandboxes[key] = env
	if err := r.Log(gungnir.Event{Name: gungnir.EventSandboxUp, Note: fmt.Sprintf("%s: container %.12s", key, env.Container)}); err != nil {
		return nil, err
	}
	if err := r.Save(); err != nil {
		return nil, err
	}
	return gungnir.Bind(r.Sandbox, env), nil
}

// SnapshotError is a snapshot that could not be made: a failure of the node, not of the sandbox.
type SnapshotError struct{ Err error }

func (e *SnapshotError) Error() string {
	return "the snapshot of the project could not be made: " + e.Err.Error()
}
func (e *SnapshotError) Unwrap() error { return e.Err }

// snapshotSources are the repos a snapshot is made of: the project umbrella, then each leaf of the project
// that is checked out.
func (r *Run) snapshotSources(ctx context.Context, revs map[string]string) ([]snapshot.Source, error) {
	p := r.Setup.Project
	sources := []snapshot.Source{{Rel: ".", Repo: git.Repo{GitDir: p.GitDir, WorkTree: p.Dir}}}
	for _, path := range r.Setup.projectLeaves() {
		nodes, err := tree.Resolve(ctx, r.Setup.Root, filepath.Join(r.Setup.ProjectPath, path))
		if err != nil {
			return nil, err
		}
		n := nodes[len(nodes)-1]
		if !n.Present {
			continue
		}
		sources = append(sources, snapshot.Source{Rel: path, Repo: git.Repo{GitDir: n.GitDir, WorkTree: n.Dir}, Rev: revs[path]})
	}
	return sources, nil
}

// DownSandboxes stops every sandbox this process or an earlier one started for the run, and clears
// the record of each one it stopped. A sandbox that won't stop stays recorded, for doctor.
func (r *Run) DownSandboxes(ctx context.Context) error {
	if r.Sandbox == nil || len(r.State.Sandboxes) == 0 {
		return nil
	}
	var errs []error
	for _, repo := range slices.Sorted(maps.Keys(r.State.Sandboxes)) {
		env := r.State.Sandboxes[repo]
		if err := r.Sandbox.Down(ctx, env); err != nil {
			errs = append(errs, fmt.Errorf("stopping the sandbox of %s: %w", repo, err))
			continue
		}
		delete(r.State.Sandboxes, repo)
		if err := r.Log(gungnir.Event{Name: gungnir.EventSandboxDown, Note: repo}); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.Save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
