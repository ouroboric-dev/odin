package run

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/odin/internal/snapshot"
)

// Grader runs the hidden suite of a task in a container of its own (spec §11). Gleipnir's driver is the
// real one; tests give a fake. It is not a gungnir.Sandbox: no agent ever has a Grader, and the engine
// reaches it only in the grade stage, after every sandbox has stopped.
type Grader interface {
	Grade(ctx context.Context, g sandbox.GradeSpec) (sandbox.GradeResult, error)
}

// NewGrader builds the grader of a run. dir is a folder of the run where the driver keeps its
// configuration. Tests replace it with a fake.
type NewGrader func(s *SandboxSetup, dir string) (Grader, error)

// defaultGrader is the devcontainer driver on the engine factory.yaml names, with a configuration folder
// of its own, so that it shares nothing with the driver of the sandboxes.
func defaultGrader(s *SandboxSetup, dir string) (Grader, error) {
	return sandbox.New(s.Engine, s.Config, dir)
}

// openGrader gives the run its grader, when its config has both a grader and a sandbox section: the
// sandbox section names the engine that runs the grader's container.
func (r *Run) openGrader(open NewGrader) error {
	if r.Setup.Config.Grader == nil || r.Setup.Sandbox == nil {
		return nil
	}
	if open == nil {
		open = defaultGrader
	}
	var err error
	r.Grader, err = open(r.Setup.Sandbox, filepath.Join(r.Dir, "grader"))
	return err
}

// GradeDir is the folder of the run that holds what grading makes: the exported /app, the logs of each
// grading run and its container's output. It is in the run folder, which no sandbox mounts.
func (r *Run) GradeDir() string { return filepath.Join(r.Dir, "grade") }

// ExportApp writes the project as it is committed to dst: the files of the project umbrella and of each
// leaf at the commit the umbrella records, with no git directory and nothing a working copy ignores
// (the snapshot of v0-T238). dst is emptied first. The result is what a grader's /app holds.
func (r *Run) ExportApp(ctx context.Context, dst string) error {
	sources, err := r.snapshotSources(ctx, nil)
	if err != nil {
		return &SnapshotError{err}
	}
	if err := snapshot.Export(ctx, dst, sources); err != nil {
		return &SnapshotError{err}
	}
	return nil
}

// NoSandboxesLeft refuses to go on while a sandbox of the run is recorded as up: grading starts only
// after every agent sandbox has stopped (spec §11).
func (r *Run) NoSandboxesLeft() error {
	if len(r.State.Sandboxes) > 0 {
		return fmt.Errorf("%d sandbox(es) of the run are still up; grading starts only after every sandbox has stopped (ygg doctor stops them)", len(r.State.Sandboxes))
	}
	return nil
}
