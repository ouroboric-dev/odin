package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/realm"
)

// sandbox runs ygg sandbox build: the one command that pulls and builds images. A run never does
// either: it refuses when an image is missing, so the founder's registry access and a run's work
// never share a step (spec §16 item 7).
func (c *command) sandbox(args []string) int {
	if len(args) == 0 || args[0] != "build" {
		fmt.Fprint(c.err, usage)
		return ExitRefused
	}
	f, _, ok := c.parse("sandbox build", args[1:], 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
		fs.Func("realm", "a realm whose sandbox stack is built: a name found in factory.yaml's realms.dir, or a directory; repeat it for more", func(v string) error {
			f.realms = append(f.realms, v)
			return nil
		})
		fs.BoolVar(&f.all, "all", false, "build the stack of every realm in factory.yaml's realms.dir")
	})
	if !ok {
		return ExitRefused
	}
	if len(f.realms) == 0 && !f.all || len(f.realms) > 0 && f.all {
		return c.refuse(errors.New("sandbox build needs --realm (once for each realm) or --all, not both"))
	}
	cfg, err := schema.LoadFactory(f.config)
	if err != nil {
		return c.refuse(err)
	}
	if cfg.Sandbox == nil {
		return c.refuse(fmt.Errorf("%s has no sandbox section", f.config))
	}
	stacks, skipped, err := stacksToBuild(cfg, f.realms, f.all)
	if err != nil {
		return c.refuse(err)
	}
	for _, s := range skipped {
		fmt.Fprintf(c.out, "%s\n", s)
	}
	work, err := os.MkdirTemp("", "ygg-sandbox-build-")
	if err != nil {
		return c.refuse(err)
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(cfg.Sandbox.Engine, sandbox.Config{}, work)
	if err != nil {
		return c.refuse(err)
	}
	for _, stack := range stacks {
		fmt.Fprintf(c.out, "Pulling and building the images for %s; this takes a few minutes.\n", stack)
		imgs, err := d.Build(c.ctx, stack)
		if err != nil {
			fmt.Fprintf(c.err, "ygg: the build failed: %v\n", err)
			return ExitFailed
		}
		fmt.Fprintf(c.out, "Built:\n  %s\n  %s\n", imgs.Work, imgs.Firewall)
	}
	return ExitDone
}

// stacksToBuild returns the distinct sandbox stacks of the realms named, in the order named, or of every
// realm in the store with --all. A realm named on purpose that has no stack is an error; with --all it
// is skipped, and the skip is returned for the output.
func stacksToBuild(cfg *schema.Factory, refs []string, all bool) (stacks, skipped []string, err error) {
	var store *realm.Store
	if cfg.Realms != nil {
		store = &realm.Store{Dir: cfg.Realms.Dir}
	}
	var dirs []string
	if all {
		if store == nil {
			return nil, nil, errors.New("--all needs factory.yaml's realms section, which says where the realm leaves are")
		}
		entries, err := os.ReadDir(store.Dir)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(store.Dir, e.Name(), "pack.yaml")); err == nil {
				dirs = append(dirs, filepath.Join(store.Dir, e.Name()))
			}
		}
	}
	for _, ref := range refs {
		dir, _, err := realm.Resolve(ref, store)
		if err != nil {
			return nil, nil, err
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		pack, err := schema.LoadPack(dir)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case pack.Sandbox == nil && all:
			skipped = append(skipped, fmt.Sprintf("The realm %s names no sandbox stack; skipped.", pack.Name))
		case pack.Sandbox == nil:
			return nil, nil, fmt.Errorf("the realm %s names no sandbox stack in its pack.yaml", pack.Name)
		case !slices.Contains(stacks, pack.Sandbox.Stack):
			stacks = append(stacks, pack.Sandbox.Stack)
		}
	}
	return stacks, skipped, nil
}
