// Package tree walks the instance tree (spec §3.1): umbrellas, which list their child nodes in
// umbrella.yaml, and leaves. It walks any depth, and checks out only the submodules on the path to
// the node it is asked for (spec §3.3 rule 6). Submodules an umbrella doesn't list as children
// are not nodes, and are never touched. A tree's root is the umbrella with root: true: a project,
// or a company umbrella holding projects.
package tree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/git"
)

// Node is a node of the instance tree.
type Node struct {
	Path       string             // relative to the root; "" is the root
	Dir        string             // its working copy
	GitDir     string             // the root's .git, or <parent's git dir>/modules/<name>
	Kind       schema.ChildKind   // the root is an umbrella
	Owner      string             // as its parent records it; the root's own
	Visibility gungnir.Visibility // likewise
	Umbrella   *schema.Umbrella   // an umbrella's umbrella.yaml; nil for a leaf, or one not checked out
	Present    bool               // checked out
}

// FindRoot walks up from dir, a node of the tree, to the root: every umbrella on the way must list
// the node below it, and the top one sets root: true. It returns the root and dir's path from it.
func FindRoot(ctx context.Context, dir string) (root, rel string, err error) {
	child, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	if u, err := schema.LoadUmbrella(child); err == nil && u.Root {
		return child, "", nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}
	for cur := child; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", fmt.Errorf("%s is not inside an instance tree: no umbrella.yaml with root: true above it", dir)
		}
		cur = parent
		u, err := schema.LoadUmbrella(parent)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a plain directory, such as repos/
		}
		if err != nil {
			return "", "", err
		}
		p, err := filepath.Rel(parent, child)
		if err != nil {
			return "", "", err
		}
		if _, ok := u.Children[p]; !ok {
			return "", "", fmt.Errorf("the umbrella %s lists no child %s", parent, p)
		}
		rel = filepath.Join(p, rel)
		if u.Root {
			return parent, rel, nil
		}
		child = parent
	}
}

// Resolve returns the nodes from root down to the node at rel, checking out any that aren't,
// and nothing else.
func Resolve(ctx context.Context, root, rel string) ([]Node, error) {
	if rel != "" && (!filepath.IsLocal(rel) || filepath.Clean(rel) != rel) {
		return nil, fmt.Errorf("%q is not a clean path inside the tree", rel)
	}
	top, err := rootNode(ctx, root)
	if err != nil {
		return nil, err
	}
	nodes := []Node{top}
	for rest := rel; rest != ""; {
		cur := nodes[len(nodes)-1]
		if cur.Kind == schema.ChildLeaf {
			return nil, fmt.Errorf("%s is a leaf; the tree has nothing below it", cur.Path)
		}
		var next string
		for _, p := range cur.Umbrella.Paths() {
			if rest == p || strings.HasPrefix(rest, p+"/") {
				next = p
			}
		}
		if next == "" {
			return nil, fmt.Errorf("the umbrella %q lists no child for %s", cur.Path, rest)
		}
		c, err := child(ctx, cur, next, true)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, c)
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, next), "/")
	}
	return nodes, nil
}

func rootNode(ctx context.Context, root string) (Node, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Node{}, err
	}
	u, err := schema.LoadUmbrella(root)
	if err != nil {
		return Node{}, err
	}
	if !u.Root {
		return Node{}, fmt.Errorf("%s is not an instance root: its umbrella.yaml doesn't set root: true", root)
	}
	gitDir, err := git.Run(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Node{}, err
	}
	return Node{Dir: root, GitDir: gitDir, Kind: schema.ChildUmbrella, Owner: u.Owner, Visibility: u.Visibility,
		Umbrella: u, Present: true}, nil
}

// child returns parent's child at path p, checking it out first if checkout is set.
func child(ctx context.Context, parent Node, p string, checkout bool) (Node, error) {
	rec := parent.Umbrella.Children[p]
	name, url, err := submodule(ctx, parent.Dir, p)
	if err != nil {
		return Node{}, err
	}
	if err := git.CheckURL(url); err != nil {
		return Node{}, fmt.Errorf("%s: submodule %s: %w", filepath.Join(parent.Dir, ".gitmodules"), p, err)
	}
	st, err := git.Run(ctx, parent.Dir, "submodule", "status", "--", p)
	if err != nil {
		return Node{}, err
	}
	present := !strings.HasPrefix(st, "-")
	if !present && checkout {
		// Only this submodule, and not recursively (spec §3.3 rules 5 and 6).
		if _, err := git.Run(ctx, parent.Dir, "submodule", "update", "--init", "--", p); err != nil {
			return Node{}, err
		}
		present = true
	}
	n := Node{Path: filepath.Join(parent.Path, p), Dir: filepath.Join(parent.Dir, p),
		GitDir: filepath.Join(parent.GitDir, "modules", name), Kind: rec.Kind, Owner: rec.Owner,
		Visibility: rec.Visibility, Present: present}
	if present && rec.Kind == schema.ChildUmbrella {
		if n.Umbrella, err = schema.LoadUmbrella(n.Dir); err != nil {
			return Node{}, fmt.Errorf("%s is listed as an umbrella: %w", n.Path, err)
		}
	}
	return n, nil
}

// submodule returns the name and URL that dir's .gitmodules records for path p.
func submodule(ctx context.Context, dir, p string) (name, url string, err error) {
	out, err := git.Run(ctx, dir, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil && !strings.Contains(err.Error(), "exit status 1") { // exit 1: no submodules at all
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(line, " ")
		if value == p {
			name = strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("the umbrella %s lists %s as a child, but has no submodule there", dir, p)
	}
	url, err = git.Run(ctx, dir, "config", "-f", ".gitmodules", "--get", "submodule."+name+".url")
	return name, url, err
}
