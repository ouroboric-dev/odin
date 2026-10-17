package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/remote"
	"github.com/ouroboric-dev/odin/internal/run"
)

// runInit creates the leaves the spec stage named, each from the template of the realm that provides it,
// and records each leaf's realm and version in the project (spec §4.2, change realm-leaf-naming). The
// operator approved the leaves file at the spec gate; here Odin reads it exactly, checks that the realms
// it names agree with the project's pins and offer what a leaf needs, and only then creates anything.
// A leaf the project has already must be pinned as the file says; it is left as it is. Run again after
// a stop, the node finds what it made and finishes the rest.
func runInit(ctx context.Context, r *run.Run, n schema.Node) error {
	attempt, err := startAttempt(r, n)
	if err != nil {
		return err
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventNodeStarted, Node: n.ID, Attempt: attempt}); err != nil {
		return err
	}
	spec, err := readLeaves(r, n)
	if err != nil {
		return &nodeFailure{err}
	}
	// The realms are loaded and the leaves file is checked against them before the run takes the realms
	// or anything is created.
	set, err := r.PlanRealms(spec.Pins())
	if err != nil {
		return &nodeFailure{err}
	}
	if err := checkLeaves(r, set, spec); err != nil {
		return &nodeFailure{err}
	}
	if err := r.AddRealms(spec.Pins()); err != nil {
		return &nodeFailure{err}
	}
	var created, kept []string
	for _, p := range initOrder(spec) {
		made, err := initLeaf(ctx, r, n, p, spec.Leaves[p])
		if err != nil {
			return err
		}
		if made {
			created = append(created, p)
		} else {
			kept = append(kept, p)
		}
	}
	r.State.Summaries[n.ID] = fmt.Sprintf("created %d leaf(s) (%s); %d already there (%s)", len(created), strings.Join(created, ", "), len(kept), strings.Join(kept, ", "))
	return r.Log(gungnir.Event{Name: gungnir.EventNodeFinished, Node: n.ID, Attempt: attempt, Result: r.State.Summaries[n.ID]})
}

// maxLeavesFile is the biggest leaves file Odin reads: a few leaves, each a few lines.
const maxLeavesFile = 1 << 20

// readLeaves reads and parses the leaves file n names.
func readLeaves(r *run.Run, n schema.Node) (*schema.Leaves, error) {
	data, err := readLeavesFile(r, n.From)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("the leaves file %s is not there as a regular file; the spec stage names the product's leaves in it", n.From)
	case err != nil:
		return nil, fmt.Errorf("the leaves file %s can't be read: %w", n.From, err)
	}
	return schema.ParseLeaves(n.From, data)
}

// readLeavesFile reads the leaves file at rel in the project, for init and for the tasks node alike: a
// regular file reached with no symlink, of at most maxLeavesFile bytes. A longer one is refused, never
// parsed cut short (review M-3).
func readLeavesFile(r *run.Run, rel string) ([]byte, error) {
	file := filepath.Join(r.Project, filepath.FromSlash(rel))
	if err := noSymlinks(r.Project, file); err != nil {
		return nil, err
	}
	listed, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	return readRegular(r.Project, filepath.FromSlash(rel), listed, maxLeavesFile)
}

// checkLeaves checks, before anything is created, that each leaf can be: its realm offers a leaf recipe,
// one that exists already is pinned as the file says, a new one is neither inside a child the project has
// nor holds one, and what a leaf consumes will exist. (The file's own paths are checked against each other
// when it is parsed.)
func checkLeaves(r *run.Run, set *realm.Set, spec *schema.Leaves) error {
	var problems []string
	u := r.Setup.Project.Umbrella
	have := u.Children
	for _, p := range spec.Paths() {
		if _, ok := have[p]; !ok {
			for _, q := range u.Paths() {
				if strings.HasPrefix(p, q+"/") || strings.HasPrefix(q, p+"/") {
					problems = append(problems, fmt.Sprintf("%s: it would be inside %s or hold it, and a child of the project is never inside another", p, q))
				}
			}
		}
		l := spec.Leaves[p]
		rl := set.Realms[l.Realm]
		if rl.Pack.Leaf == nil {
			problems = append(problems, fmt.Sprintf("%s: the realm %s offers no leaf recipe (a template, an implementer prompt and checks), so it can't provide a leaf", p, l.Realm))
		}
		if c, ok := have[p]; ok {
			realmName, version := c.Realm, c.RealmVersion
			if realmName == "" { // a leaf with no pin is the lead realm's
				realmName, version = r.Setup.Lead.Name, r.Setup.Lead.Version
			}
			switch {
			case c.Kind != schema.ChildLeaf:
				problems = append(problems, fmt.Sprintf("%s: the project has an umbrella there, not a leaf", p))
			case realmName != l.Realm || version != l.RealmVersion:
				problems = append(problems, fmt.Sprintf("%s: the project has this leaf from %s %s, and the leaves file names %s %s", p, realmName, version, l.Realm, l.RealmVersion))
			case !slices.Equal(c.Consumes, l.Consumes):
				problems = append(problems, fmt.Sprintf("%s: the project has this leaf consuming %v, and the leaves file says %v; rewiring a leaf that exists is not something a run does", p, c.Consumes, l.Consumes))
			}
		}
		for _, q := range l.Consumes {
			if _, listed := spec.Leaves[q]; !listed && have[q].Kind != schema.ChildLeaf {
				problems = append(problems, fmt.Sprintf("%s: it consumes %s, which neither the file nor the project has as a leaf", p, q))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the leaves file can't be made real, so nothing was created:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// initOrder puts the leaves a leaf consumes before it, since a remote records a leaf's consumers only if
// they exist; otherwise the order of the paths.
func initOrder(spec *schema.Leaves) []string {
	var out []string
	done := map[string]bool{}
	var visit func(p string)
	visit = func(p string) {
		if done[p] {
			return
		}
		done[p] = true
		for _, q := range spec.Leaves[p].Consumes {
			if _, listed := spec.Leaves[q]; listed {
				visit(q)
			}
		}
		out = append(out, p)
	}
	for _, p := range spec.Paths() {
		visit(p)
	}
	return out
}

// initLeaf makes leaf p real: it creates it through the remote when the project lacks it, makes it part
// of the run, and, for a leaf this run created that still holds only its first commit, adds the rest of
// the realm's template to the run's branch in a checkpoint of its own. A leaf the project had before the
// run is left as it is, even with one commit (review M-8): run.json records each leaf init creates before
// it is created, so a resume finishes the template of its own leaves and of no other. It reports whether
// it created the leaf.
func initLeaf(ctx context.Context, r *run.Run, n schema.Node, p string, l schema.LeafSpec) (created bool, err error) {
	s := r.Setup
	rl := s.Realms.Realms[l.Realm]
	recipe := rl.Pack.Leaf
	if _, has := s.Project.Umbrella.Children[p]; !has {
		r.State.NewLeaves[p] = true
		if err := r.Save(); err != nil {
			return false, err
		}
		files := map[string][]byte{}
		for _, f := range recipe.FirstCommit {
			b, err := os.ReadFile(filepath.Join(rl.Dir, recipe.Template, f))
			if err != nil {
				return false, err
			}
			files[f] = b
		}
		repo := gungnir.Repo{Owner: s.Project.Owner, Name: filepath.Base(p), Visibility: s.Project.Visibility, Description: l.Description,
			Path: p, Files: files, Realm: l.Realm, RealmVersion: l.RealmVersion, Consumes: l.Consumes}
		rem := remote.NewLocal(s.Project)
		if err := rem.Create(ctx, repo); err != nil {
			return false, &nodeFailure{err}
		}
		created = true
		if err := r.Log(gungnir.Event{Name: gungnir.EventRepoCreated, Node: n.ID, Realm: l.Realm, RealmVersion: l.RealmVersion,
			Note: fmt.Sprintf("%s (%s remote)", p, rem.Name())}); err != nil {
			return created, err
		}
	}
	if err := r.AddLeaf(ctx, p); err != nil {
		return created, &nodeFailure{err}
	}
	lf, err := r.Leaf(p)
	if err != nil {
		return created, err
	}
	if !r.State.NewLeaves[p] {
		return created, nil // a leaf of the project's, not of this run's: its files are its own
	}
	if count, err := lf.Git(ctx, "rev-list", "--count", "HEAD"); err != nil || count != "1" {
		if err == nil {
			// Its template was applied. A stop between the checkpoint and the move of main is finished here.
			if rec := r.State.Leaves[p]; rec.Checkpoint != "" {
				if subj, _ := lf.Git(ctx, "log", "-1", "--format=%s", rec.Checkpoint); subj == templateMessage(r, n, l) {
					err = mainToTemplate(ctx, r, lf, p, rec.Checkpoint)
				}
			}
		}
		return created, err
	}
	if err := copyTemplate(filepath.Join(rl.Dir, recipe.Template), lf.Dir(), recipe.FirstCommit); err != nil {
		return created, &nodeFailure{fmt.Errorf("copying the template of %s into %s: %w", l.Realm, p, err)}
	}
	head, _, err := lf.Checkpoint(ctx, templateMessage(r, n, l))
	if err != nil {
		return created, err
	}
	if err := mainToTemplate(ctx, r, lf, p, head); err != nil {
		return created, err
	}
	rec := r.State.Leaves[p]
	rec.Checkpoint = head
	r.State.Leaves[p] = rec
	return created, r.Save()
}

func templateMessage(r *run.Run, n schema.Node, l schema.LeafSpec) string {
	return fmt.Sprintf("ygg: run %s, node %s: the %s template", r.State.ID, n.ID, l.Realm)
}

// mainToTemplate moves the leaf's main to the template's checkpoint, under the leaf's lock and with
// update-ref as a merge does, so that main holds the template before any task starts: the checks and the
// reviewers take "the commit the task started from" to be main, and a blocked task resets to it (final
// review I-2). main moves only forward, from where it is to a commit that holds it.
func mainToTemplate(ctx context.Context, r *run.Run, lf *leaf.Leaf, repo, head string) error {
	release, err := lock.Take(r.LockFile(repo))
	if err != nil {
		return err
	}
	defer release()
	main, err := lf.Rev(ctx, "main")
	if err != nil || main == head {
		return err
	}
	if _, err := lf.Git(ctx, "merge-base", "--is-ancestor", main, head); err != nil {
		return nil // main has moved on its own: the template is not put over it
	}
	_, err = lf.Git(ctx, "update-ref", "refs/heads/main", head, main)
	return err
}

// copyTemplate copies the template's files into the leaf, except the first commit's, which the leaf has.
// A template is the realm's own text; a link or any file but a regular one in it is refused. Nothing is
// written through a symlink in the leaf, or over anything in it but a regular file: the write would land
// outside the leaf, or not where the template says.
func copyTemplate(tmpl, dst string, first []string) error {
	return filepath.WalkDir(tmpl, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tmpl, p)
		if err != nil || rel == "." {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if err := noSymlinks(dst, target); err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is %s, not a regular file", p, d.Type())
		}
		if slices.Contains(first, filepath.ToSlash(rel)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if st, err := os.Lstat(target); err == nil && !st.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file", target)
		}
		return os.WriteFile(target, data, mode)
	})
}
