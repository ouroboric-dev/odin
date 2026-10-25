package engine

import (
	"bytes"
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
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/pointer"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/remote"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
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
	set, runtime, leafRealms, err := planLeafRealms(r, spec)
	if err != nil {
		return &nodeFailure{err}
	}
	if err := checkLeaves(r, set, spec); err != nil {
		return &nodeFailure{err}
	}
	// Acceptance checks run over the leaves' realms, so a move that asks for one needs a realm of the leaves
	// to define it, and a stack to run it on, before any leaf is created (change voluspa). The needs of the
	// leaves' realms need a runtime realm that provides them (spec §6.1).
	if err := r.Setup.CheckAcceptance(leafRealms); err != nil {
		return &nodeFailure{err}
	}
	if err := r.Setup.CheckNeedsWith(runtime, leafRealms); err != nil {
		return &nodeFailure{err}
	}
	// The realms of a fresh run enter here, after run start checked the prompts of the realms it had: their
	// implementer prompts name only the inputs the loops declare (ruling R66).
	if err := run.CheckRealmPrompts(r.Setup.Graph, leafRealms); err != nil {
		return &nodeFailure{err}
	}
	if err := r.AddRealms(leavesPins(spec)); err != nil {
		return &nodeFailure{err}
	}
	if err := ignoreFactory(ctx, r.Project); err != nil {
		return &nodeFailure{err}
	}
	if err := pinRuntime(ctx, r, spec); err != nil {
		return &nodeFailure{err}
	}
	if err := writeProductStart(ctx, r.Project, runtime); err != nil {
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

// leavesPins are the realm pins of a leaves file: each leaf's, and its runtime pin.
func leavesPins(spec *schema.Leaves) []schema.Pin {
	pins := spec.Pins()
	if p, ok := spec.RuntimePin(); ok {
		pins = append(pins, p)
	}
	return pins
}

// planLeafRealms loads, and records nothing of, the realms a leaves file names: the set the run would have, the
// runtime realm the project would have (the file's, else the one the umbrella records, or nil), and the realm of
// each leaf of the file, in path order. A runtime pin other than the one the project records is refused: a
// project pins one runtime realm, and once recorded a leaves file names it or none (spec §9.4).
func planLeafRealms(r *run.Run, spec *schema.Leaves) (*realm.Set, *realm.Realm, []*realm.Realm, error) {
	if pin, ok := spec.RuntimePin(); ok {
		if have, recorded := r.Setup.Project.Umbrella.RuntimePin(); recorded && (have.Realm != pin.Realm || have.Version != pin.Version) {
			return nil, nil, nil, fmt.Errorf("the leaves file pins the runtime realm %s %s, and the project records %s %s (runtime in %s): a project pins one runtime realm, so a leaves file names that pin or none",
				pin.Realm, pin.Version, have.Realm, have.Version, schema.UmbrellaFile)
		}
	}
	set, runtime, err := r.PlanRealms(leavesPins(spec))
	if err != nil {
		return nil, nil, nil, err
	}
	var leaves []*realm.Realm
	for _, p := range spec.Paths() {
		leaves = append(leaves, set.Realms[spec.Leaves[p].Realm])
	}
	return set, runtime, leaves, nil
}

// pinRuntime records the runtime realm the leaves file pins in the project's umbrella.yaml (runtime and
// runtime_version), when the project records none yet (spec §9.4). The commit that records the first leaf init
// creates records the pin too; with no leaf to create, the pin gets a commit of its own. A pin the umbrella has
// already adds nothing, so a resumed init adds no commit. The operator's own uncommitted edit of umbrella.yaml is
// not Odin's to commit.
func pinRuntime(ctx context.Context, r *run.Run, spec *schema.Leaves) error {
	pin, ok := spec.RuntimePin()
	u := r.Setup.Project.Umbrella
	if !ok || u.Runtime != "" {
		return nil
	}
	if st, err := git.Run(ctx, r.Project, "status", "--porcelain", "--", schema.UmbrellaFile); err != nil || st != "" {
		return fmt.Errorf("%s has changes that are not committed, so init can't pin the runtime realm %s %s in it: commit or discard them first (%v)",
			filepath.Join(r.Project, schema.UmbrellaFile), pin.Realm, pin.Version, err)
	}
	u.Runtime, u.RuntimeVersion = pin.Realm, pin.Version
	for _, p := range spec.Paths() {
		if _, has := u.Children[p]; !has {
			return nil // the commit that creates this leaf records the pin
		}
	}
	if err := u.Save(r.Project); err != nil {
		return err
	}
	_, err := git.Run(ctx, r.Project, "commit", "-q", "-m", fmt.Sprintf("ygg init: pin the runtime realm %s %s", pin.Realm, pin.Version), "--", schema.UmbrellaFile)
	return err
}

// factoryIgnore is the line the project's .gitignore needs, so the run folders under .factory/ do not
// show in its status (backlog B-073).
const factoryIgnore = ".factory/"

// ignoreFactory makes the project's .gitignore name .factory/ and commits that.
func ignoreFactory(ctx context.Context, project string) error {
	return ignoreLine(ctx, project, factoryIgnore)
}

// ignoreLine makes the project's .gitignore hold the line and commits that. It creates the file
// when the project has none, adds the line to one that lacks it, and leaves one that has it alone, so
// a resumed init adds nothing twice. It refuses when the file has changes that are not committed.
func ignoreLine(ctx context.Context, project, line string) error {
	path := filepath.Join(project, ".gitignore")
	// The operator's own uncommitted edit of .gitignore is not Odin's to commit.
	st, err := git.Run(ctx, project, "status", "--porcelain", "--", ".gitignore")
	if err != nil {
		return err
	}
	if st != "" {
		return fmt.Errorf("%s has changes that are not committed, so init can't add %s to it: commit or discard them first", path, line)
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, have := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(have) == line {
			return nil
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, append(b, line+"\n"...), 0o644); err != nil {
		return err
	}
	if _, err := git.Run(ctx, project, "add", "--", ".gitignore"); err != nil {
		return err
	}
	_, err = git.Run(ctx, project, "commit", "-q", "-m", "ygg init: ignore "+line, "--", ".gitignore")
	return err
}

// productStartFile is where the project keeps the runtime realm's product start, at its root, and dataIgnore
// is the line its .gitignore needs: the product's data folder is not source.
const (
	productStartFile = "start.sh"
	dataIgnore       = ".data/"
)

// writeProductStart writes the runtime realm's product start script as start.sh at the project's root, mode
// 0755, and commits it; it adds .data/ to the project's .gitignore. A project with no runtime realm, or whose
// runtime realm names no product start, keeps its own start.sh. A start.sh that is already the realm's, with
// the mode, adds no commit, so a resumed init adds nothing; one that differs gets the realm's in a new commit
// that says so. The mode is set by hand, since a file Odin writes through the remote loses it (B-128).
func writeProductStart(ctx context.Context, project string, runtime *realm.Realm) error {
	if runtime == nil || runtime.Pack.Product == nil {
		return nil
	}
	want, err := os.ReadFile(filepath.Join(runtime.Dir, runtime.Pack.Product.Start))
	if err != nil {
		return fmt.Errorf("the runtime realm %s names the product start %s, and its folder has no such file: %w", runtime.Name, runtime.Pack.Product.Start, err)
	}
	path := filepath.Join(project, productStartFile)
	if err := noSymlinks(project, path); err != nil {
		return fmt.Errorf("init can't write the runtime realm's product start: %w", err)
	}
	have, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	exists := err == nil
	st, _ := os.Stat(path)
	same := exists && bytes.Equal(have, want) && st.Mode().Perm() == 0o755
	changes, err := git.Run(ctx, project, "status", "--porcelain", "--", productStartFile)
	if err != nil {
		return err
	}
	if same && changes == "" { // the realm's file, tracked as it is
		return ignoreLine(ctx, project, dataIgnore)
	}
	if !same && changes != "" {
		return fmt.Errorf("%s has changes that are not committed, so init can't write the runtime realm's product start to it: commit or discard them first", path)
	}
	if !same { // else the file is the realm's already, untracked or not committed as it is, and only needs its commit
		if err := os.WriteFile(path, want, 0o755); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	msg := fmt.Sprintf("ygg init: write %s, the product start of the runtime realm %s %s", productStartFile, runtime.Name, runtime.Version)
	if exists && !same {
		msg = fmt.Sprintf("ygg init: replace %s with the product start of the runtime realm %s %s", productStartFile, runtime.Name, runtime.Version)
	}
	if _, err := git.Run(ctx, project, "add", "--", productStartFile); err != nil {
		return err
	}
	if _, err := git.Run(ctx, project, "commit", "-q", "-m", msg, "--", productStartFile); err != nil {
		return err
	}
	return ignoreLine(ctx, project, dataIgnore)
}

// maxLeavesFile is the biggest leaves file Odin reads: a few leaves, each a few lines.
const maxLeavesFile = 1 << 20

// readLeaves reads and parses the leaves file n names: the one Odin wrote from the spec's typed leaves when it
// published the spec, read back as data Odin wrote, not as anything an agent wrote.
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
			if realmName == "" { // a leaf with no pin is the graph pack's when it holds a recipe, else refused
				g, err := r.Setup.RealmOf(p)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", p, err))
					continue
				}
				realmName, version = g.Name, g.Version
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
					err = templateToMain(ctx, r, lf, p, rec.Checkpoint)
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
	// The checkpoint is saved before main moves and the umbrella records it, so a stop or a refusal in
	// either step is finished by the resume above, which finds the template by the checkpoint.
	rec := r.State.Leaves[p]
	rec.Checkpoint = head
	r.State.Leaves[p] = rec
	if err := r.Save(); err != nil {
		return created, err
	}
	return created, templateToMain(ctx, r, lf, p, head)
}

func templateMessage(r *run.Run, n schema.Node, l schema.LeafSpec) string {
	return fmt.Sprintf("ygg: run %s, node %s: the %s template", r.State.ID, n.ID, l.Realm)
}

// templateToMain moves the leaf's main to the template's commit, then records that commit in the leaf's
// parent umbrella. The acceptance snapshot takes every leaf at the commit its umbrella records, so a new
// leaf recorded at its first commit would be seen without its template by the first pointer move of the
// run (B-061). The record is the one commit of the host that a pointer move makes, with nothing to check
// (the move to the leaf's template is the realm's own text), and a leaf already recorded there makes none,
// so a resume that stopped between the two steps finishes it (the caller saved the checkpoint first). The
// record does not mean "passed acceptance": it only makes the snapshot hold the template. It is refused,
// as a pointer move is, when the umbrella has staged changes Odin didn't make.
func templateToMain(ctx context.Context, r *run.Run, lf *leaf.Leaf, repo, head string) error {
	if err := mainToTemplate(ctx, r, lf, repo, head); err != nil {
		return err
	}
	if main, err := lf.Rev(ctx, "main"); err != nil || main != head {
		return err // main moved on its own: the template is not recorded over it
	}
	nodes, err := tree.Resolve(ctx, r.Setup.Root, filepath.Join(r.Setup.ProjectPath, repo))
	if err != nil {
		return err
	}
	parent, leafNode := nodes[len(nodes)-2], nodes[len(nodes)-1]
	child, err := filepath.Rel(parent.Dir, leafNode.Dir)
	if err != nil {
		return err
	}
	parentRepo, err := filepath.Rel(r.Project, parent.Dir)
	if err != nil {
		return err
	}
	ok := func(context.Context) error { return nil }
	_, err = pointer.Move(ctx, pointer.Spec{Parent: parent, Child: child, Leaf: lf, To: head, Lock: r.LockFile(parentRepo),
		Check: ok, Acceptance: ok, Message: fmt.Sprintf("Record the template of %s at %.12s (run %s)", child, head, r.State.ID)})
	return err
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
