package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/realm"
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
// sandbox holds the workspace and nothing else. It has no stack, so it starts on the factory layer, and its
// only egress is the model APIs of the run's roles. It is recorded under the node's scratch name so that
// Advance, a resume and doctor stop it like a leaf's.
func (r *Run) ScratchRunner(ctx context.Context, node string) (gungnir.Runner, error) {
	if r.Sandbox == nil {
		return nil, nil
	}
	key := "scratch/" + node
	if env, ok := r.State.Sandboxes[key]; ok {
		return gungnir.Bind(r.Sandbox, env), nil
	}
	env, err := r.Sandbox.Up(ctx, gungnir.SandboxSpec{Run: r.State.ID, Name: key, Parent: r.Dir, Leaf: r.ScratchDir(node), Scratch: true,
		Egress: r.Setup.Sandbox.Hosts})
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
// acceptance checks of realm rl for node: the committed files of the project umbrella and of each leaf, at the commits
// they are on, or at the commits revs names for a leaf (by its path), in a plain folder of the run. The
// snapshot is made again every time, since the work it holds changes between attempts; the sandbox
// is started the first time, on rl's acceptance stack with the services of every leaf's realm, and is recorded under its own name (snapshot/<node>/<realm>)
// so that Advance, a resume and doctor stop it like a leaf's (change realm-content).
func (r *Run) SnapshotRunner(ctx context.Context, node string, rl *realm.Realm, revs map[string]string) (gungnir.Runner, error) {
	return r.snapshotRunner(ctx, node, rl, revs, true)
}

// snapshotRunner is SnapshotRunner; withServices false starts the sandbox with no service, for a snapshot
// whose commands run no part of the product (the release's build).
func (r *Run) snapshotRunner(ctx context.Context, node string, rl *realm.Realm, revs map[string]string, withServices bool) (gungnir.Runner, error) {
	if r.Sandbox == nil {
		return nil, nil
	}
	key, dir := "snapshot/"+node+"/"+rl.Name, filepath.Join(r.Dir, "snapshot", node, rl.Name)
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
	stack, egress, services, err := r.Setup.SnapshotSandbox(rl)
	if err != nil {
		return nil, err
	}
	if !withServices {
		services = nil
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

// snapshotSources are the repos a snapshot is made of: the project umbrella, then each leaf of the project.
// A leaf is taken at the commit revs names for its path, and otherwise at the commit its parent umbrella
// records for it (the gitlink at the umbrella's HEAD), never at whatever its working copy is on. A leaf that
// is not checked out, with no record of it in its umbrella, or without the recorded commit, is an error
// that names it: a snapshot that lacks a leaf, or holds another commit of it, would pass checks that
// should fail (B-048 M-1).
func (r *Run) snapshotSources(ctx context.Context, revs map[string]string) ([]snapshot.Source, error) {
	p := r.Setup.Project
	sources := []snapshot.Source{{Rel: ".", Repo: git.Repo{GitDir: p.GitDir, WorkTree: p.Dir}}}
	byPath := map[string]tree.Node{}
	if err := tree.Walk(ctx, r.Setup.Root, func(n tree.Node) error { byPath[n.Path] = n; return nil }); err != nil {
		return nil, err
	}
	for _, path := range r.Setup.projectLeaves() {
		full := filepath.Join(r.Setup.ProjectPath, path)
		n, ok := byPath[full]
		if !ok || !n.Present {
			return nil, fmt.Errorf("the leaf %s is not checked out", path)
		}
		parent := byPath[""]
		for d := filepath.Dir(full); d != "."; d = filepath.Dir(d) {
			if u, ok := byPath[d]; ok {
				parent = u
				break
			}
		}
		repo := git.Repo{GitDir: n.GitDir, WorkTree: n.Dir}
		rev := revs[path]
		if rev == "" {
			rel, err := filepath.Rel(parent.Path, full)
			if err != nil {
				return nil, err
			}
			if rev, err = recordedCommit(ctx, git.Repo{GitDir: parent.GitDir, WorkTree: parent.Dir}, filepath.ToSlash(rel)); err != nil {
				return nil, fmt.Errorf("the leaf %s: %w", path, err)
			}
		}
		if _, err := repo.Run(ctx, "cat-file", "-e", rev+"^{commit}"); err != nil {
			return nil, fmt.Errorf("the leaf %s does not have the commit %s", path, rev)
		}
		sources = append(sources, snapshot.Source{Rel: path, Repo: repo, Rev: rev})
	}
	return sources, nil
}

// recordedCommit is the commit the umbrella repo records for its child at rel: the gitlink in its HEAD.
func recordedCommit(ctx context.Context, umbrella git.Repo, rel string) (string, error) {
	out, err := umbrella.Run(ctx, "ls-tree", "HEAD", "--", rel)
	if err != nil {
		return "", err
	}
	f := strings.Fields(strings.SplitN(out, "\t", 2)[0])
	if len(f) != 3 || f[0] != "160000" {
		return "", fmt.Errorf("its umbrella records no commit for %s", rel)
	}
	return f[2], nil
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
