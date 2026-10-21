package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/deps"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// bumpRunning reports whether a ygg holds the project's runs lock, as a bump does from its first line to its last.
func bumpRunning(project string) bool { return lock.Held(run.RunsLock(project)) }

// isBump reports whether id is a bump of the project: a bump names its sandbox by its own id.
func isBump(project, id string) bool {
	st, err := os.Stat(filepath.Join(project, ".factory", "deps", id))
	return err == nil && st.IsDir()
}

// unfinishedBumps lists the bumps that stopped before they ended, one finding each, unless a ygg is running one.
// With git true it runs the leaf guard and offers git; otherwise it only reads files and says instead, which
// is next. Status never offers git: a container of the bump may still hold the product.
func (c *command) unfinishedBumps(project string, git bool, instead string) (ids []string, msgs []string, err error) {
	if bumpRunning(project) {
		return nil, nil, nil
	}
	list, err := deps.FindUnfinished(project)
	for _, u := range list {
		ids = append(ids, u.ID)
		next := instead
		if git {
			next = c.nextForProduct(project, u.Product, u.ID)
		}
		msgs = append(msgs, fmt.Sprintf("the bump of %s stopped in %s before it ended; the product may be changed. %s", u.Package, orUnknown(u.Product), next))
	}
	return ids, msgs, err
}

func orUnknown(product string) string {
	if product == "" {
		return "a product"
	}
	return product
}

// nextForProduct is what to do next: look at what the bump left in the product, and then clear the bump's
// folder. A killed bump may have rewritten the product's .git file, and git would then run the bump's
// code on the host, so the leaf guard runs first and git is only offered when it passes. The caller has
// stopped the bump's containers, or the config has no sandbox: nothing else writes in the product. The
// sentences end in the command, with no full stop, so that it can be copied.
func (c *command) nextForProduct(project, product, id string) string {
	clear := "remove " + filepath.Join(project, ".factory", "deps", id)
	if product == "" {
		return "Look at the products the bump named. Once they are as you want them, " + clear
	}
	dir := filepath.Join(project, product)
	if err := c.guardProduct(project, product); err != nil {
		return fmt.Sprintf("The .git of the product is not as it was (%v). Do not run git in %s. Once the product is as you want it, %s", err, dir, clear)
	}
	return "Run: git -C " + dir + " diff\nThen, once the product is as you want it, " + clear
}

// guardProduct runs the leaf guard on the product, as the bump does before it starts: the .git file is the
// one git made, and no repository is planted below it.
func (c *command) guardProduct(project, product string) error {
	root, projectPath, err := tree.FindRoot(c.ctx, project)
	if err != nil {
		return err
	}
	ns, err := tree.Resolve(c.ctx, root, filepath.Join(projectPath, filepath.ToSlash(product)))
	if err != nil {
		return err
	}
	l, err := leaf.Open(ns[len(ns)-1])
	if err != nil {
		return err
	}
	return l.Guard()
}

// bumpContainers lists the containers of the project's bumps that the engine still holds. status only lists:
// ygg doctor stops them. It is empty when the config has no sandbox or the engine does not answer.
func (c *command) bumpContainers(project string, cfg *schema.Factory) []sandbox.Leftover {
	if cfg == nil || cfg.Sandbox == nil || bumpRunning(project) {
		return nil
	}
	work, err := os.MkdirTemp("", "ygg-status-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(cfg.Sandbox.Engine, sandbox.Config{}, work)
	if err != nil {
		return nil
	}
	left, err := d.Leftovers(c.ctx)
	if err != nil {
		return nil
	}
	var out []sandbox.Leftover
	for _, l := range left {
		if isBump(project, l.Run) {
			out = append(out, l)
		}
	}
	return out
}
