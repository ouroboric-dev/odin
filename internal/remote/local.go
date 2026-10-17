// Package remote holds Odin's remotes (spec §2.2; change remote-creates-repos). v0 has the local
// remote; the github remote is iteration 2.
package remote

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
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// Local creates repos in place, in an umbrella of the instance tree on this host. It needs no
// admin rights, so no repo-admin approval: the plan's approval covers it.
type Local struct {
	parent tree.Node // the umbrella new repos are created in
}

var _ gungnir.Remote = (*Local)(nil)

// NewLocal returns the local remote for the umbrella parent.
func NewLocal(parent tree.Node) *Local { return &Local{parent: parent} }

func (l *Local) Name() string { return "local" }

// Create makes r a leaf of the umbrella: a new repo at r.Path whose one commit holds r.Files, added
// as a submodule with its git dir moved into the umbrella's .git/modules, and recorded in the
// umbrella's umbrella.yaml. The umbrella gets one commit for it. The submodule's URL is the
// relative path, so the leaf lives on this host only.
func (l *Local) Create(ctx context.Context, r gungnir.Repo) error {
	if err := r.Validate(); err != nil {
		return err
	}
	p := l.parent
	if p.Kind != schema.ChildUmbrella || p.Umbrella == nil {
		return fmt.Errorf("%s is not a checked-out umbrella", p.Path)
	}
	dir := filepath.Join(p.Dir, r.Path)
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists, so nothing was created", dir)
	}
	if _, listed := p.Umbrella.Children[r.Path]; listed {
		return fmt.Errorf("the umbrella %q already lists %s, so nothing was created", p.Path, r.Path)
	}
	for q := range p.Umbrella.Children {
		if strings.HasPrefix(r.Path, q+"/") || strings.HasPrefix(q, r.Path+"/") {
			return fmt.Errorf("%s would be inside %s or hold it, and a child is never inside another, so nothing was created", r.Path, q)
		}
	}
	for _, c := range r.Consumes {
		if p.Umbrella.Children[c].Kind != schema.ChildLeaf {
			return fmt.Errorf("%s consumes %s, which %q doesn't list as a leaf, so nothing was created", r.Path, c, p.Path)
		}
	}
	if err := firstCommit(ctx, dir, r); err != nil {
		return fmt.Errorf("creating %s: %w (remove %s before trying again)", r.Path, err, dir)
	}
	steps := [][]string{
		{"submodule", "add", "-q", "./" + r.Path, r.Path},
		{"submodule", "absorbgitdirs", "--", r.Path},
	}
	for _, s := range steps {
		if _, err := git.Run(ctx, p.Dir, s...); err != nil {
			return fmt.Errorf("adding %s to %q: %w", r.Path, p.Path, err)
		}
	}
	p.Umbrella.Children[r.Path] = schema.Child{Kind: schema.ChildLeaf, Owner: r.Owner, Visibility: r.Visibility,
		Realm: r.Realm, RealmVersion: r.RealmVersion, Consumes: r.Consumes}
	if err := p.Umbrella.Save(p.Dir); err != nil {
		return err
	}
	if _, err := git.Run(ctx, p.Dir, "add", "--", ".gitmodules", schema.UmbrellaFile, r.Path); err != nil {
		return err
	}
	msg := fmt.Sprintf("Create leaf %s (%s/%s, %s)", r.Path, r.Owner, r.Name, r.Visibility)
	_, err := git.Run(ctx, p.Dir, "commit", "-q", "-m", msg, "--", ".gitmodules", schema.UmbrellaFile, r.Path)
	return err
}

// firstCommit makes the repo at dir with one commit holding r.Files.
func firstCommit(ctx context.Context, dir string, r gungnir.Repo) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := git.Run(ctx, dir, "init", "-q"); err != nil {
		return err
	}
	paths := make([]string, 0, len(r.Files))
	for f := range r.Files {
		paths = append(paths, f)
	}
	slices.Sort(paths)
	for _, f := range paths { // Validate keeps every path inside dir and outside .git
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, f), r.Files[f], 0o644); err != nil {
			return err
		}
	}
	if _, err := git.Run(ctx, dir, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	_, err := git.Run(ctx, dir, "commit", "-q", "-m", "First commit: "+r.Description)
	return err
}

// Protect does nothing: a repo on this host has no settings or rulesets to apply.
func (l *Local) Protect(ctx context.Context, r gungnir.Repo) (bool, error) { return false, nil }
