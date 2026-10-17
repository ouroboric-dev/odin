package engine

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
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
	if err := r.AddRealms(spec.Pins()); err != nil {
		return &nodeFailure{err}
	}
	if err := checkLeaves(r, spec); err != nil {
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

// readLeaves reads and parses the leaves file n names.
func readLeaves(r *run.Run, n schema.Node) (*schema.Leaves, error) {
	file := filepath.Join(r.Project, filepath.FromSlash(n.From))
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || noSymlinks(r.Project, file) != nil {
		return nil, fmt.Errorf("the leaves file %s is not there as a regular file; the spec stage names the product's leaves in it", n.From)
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	return schema.ParseLeaves(n.From, data)
}

// checkLeaves checks, before anything is created, that each leaf can be: its realm offers a leaf recipe,
// one that exists already is pinned as the file says, and what a leaf consumes will exist.
func checkLeaves(r *run.Run, spec *schema.Leaves) error {
	var problems []string
	have := r.Setup.Project.Umbrella.Children
	for _, p := range spec.Paths() {
		l := spec.Leaves[p]
		rl := r.Setup.Realms.Realms[l.Realm]
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
// of the run, and, while the leaf holds only its first commit, adds the rest of the realm's template to
// the run's branch in a checkpoint of its own. It reports whether it created the leaf.
func initLeaf(ctx context.Context, r *run.Run, n schema.Node, p string, l schema.LeafSpec) (created bool, err error) {
	s := r.Setup
	rl := s.Realms.Realms[l.Realm]
	recipe := rl.Pack.Leaf
	if _, has := s.Project.Umbrella.Children[p]; !has {
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
	if count, err := lf.Git(ctx, "rev-list", "--count", "HEAD"); err != nil || count != "1" {
		return created, err // the leaf has history beyond its first commit: its template was applied, or it is not new
	}
	if err := copyTemplate(filepath.Join(rl.Dir, recipe.Template), lf.Dir(), recipe.FirstCommit); err != nil {
		return created, &nodeFailure{fmt.Errorf("copying the template of %s into %s: %w", l.Realm, p, err)}
	}
	head, _, err := lf.Checkpoint(ctx, fmt.Sprintf("ygg: run %s, node %s: the %s template", r.State.ID, n.ID, l.Realm))
	if err != nil {
		return created, err
	}
	rec := r.State.Leaves[p]
	rec.Checkpoint = head
	r.State.Leaves[p] = rec
	return created, r.Save()
}

// copyTemplate copies the template's files into the leaf, except the first commit's, which the leaf has.
// A template is the realm's own text; a link or any file but a regular one in it is refused.
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
		return os.WriteFile(target, data, mode)
	})
}
