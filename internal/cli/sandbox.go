package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	stacks, services, skipped, err := stacksToBuild(cfg, f.realms, f.all)
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
	// The factory layer comes first: a scratch session starts on it, whatever realms the project has.
	fmt.Fprintf(c.out, "Building the factory layer on %s.\n", sandbox.FactoryStack)
	fimgs, err := d.BuildFactory(c.ctx)
	if err != nil {
		fmt.Fprintf(c.err, "ygg: building the factory layer failed: %v\n", err)
		return ExitFailed
	}
	fmt.Fprintf(c.out, "Built:\n  %s\n  %s\n", fimgs.Work, fimgs.Firewall)
	for _, st := range stacks {
		if st.dir != "" {
			fmt.Fprintf(c.out, "Building the stack %s from the realm's context.\n", st.ref)
			built, err := d.BuildStack(c.ctx, st.ref, st.dir, st.target)
			if err != nil {
				fmt.Fprintf(c.err, "ygg: building the stack failed: %v\n", err)
				return ExitFailed
			}
			if !built {
				fmt.Fprintf(c.out, "The stack %s is there already.\n", st.ref)
			}
		}
		fmt.Fprintf(c.out, "Pulling and building the images for %s; this takes a few minutes.\n", st.ref)
		imgs, err := d.Build(c.ctx, st.ref)
		if err != nil {
			fmt.Fprintf(c.err, "ygg: the build failed: %v\n", err)
			return ExitFailed
		}
		fmt.Fprintf(c.out, "Built:\n  %s\n  %s\n", imgs.Work, imgs.Firewall)
	}
	for _, image := range services {
		fmt.Fprintf(c.out, "Pulling the service image %s.\n", image)
		if err := d.Pull(c.ctx, image); err != nil {
			fmt.Fprintf(c.err, "ygg: pulling a service image failed: %v\n", err)
			return ExitFailed
		}
	}
	return ExitDone
}

// stackBuild is one stack to build: the image the layers go on (ref) and, for a stack built from a realm's
// context, the context's folder and the stage to build. dir is empty for an image pinned by digest.
type stackBuild struct{ ref, dir, target string }

// stacksToBuild returns the distinct sandbox stacks of the realms named, in the order named, or of every
// realm in the store with --all, and the distinct images of their services, which a sandbox uses without
// ever pulling them. A realm that has no stack and names no service is an error when named on purpose, and
// skipped with --all. One that names services, the runtime realm, has no stack built, and the skip is returned
// for the output, but its service images are still pulled: they are the providers of the build realms' needs.
func stacksToBuild(cfg *schema.Factory, refs []string, all bool) (stacks []stackBuild, services, skipped []string, err error) {
	var store *realm.Store
	if cfg.Realms != nil {
		store = &realm.Store{Dir: cfg.Realms.Dir, Kind: "realm"}
	}
	var dirs []string
	if all {
		if store == nil {
			return nil, nil, nil, errors.New("--all needs factory.yaml's realms section, which says where the realm leaves are")
		}
		entries, err := os.ReadDir(store.Dir)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(store.Dir, e.Name(), "pack.yaml")); err == nil {
				dirs = append(dirs, filepath.Join(store.Dir, e.Name()))
			}
		}
	}
	for _, ref := range refs {
		dir, _, err := realm.Resolve(ref, store, "realm")
		if err != nil {
			return nil, nil, nil, err
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		pack, err := schema.LoadPack(dir)
		if err != nil {
			return nil, nil, nil, err
		}
		switch {
		case pack.Sandbox == nil && len(pack.Services) > 0:
			skipped = append(skipped, fmt.Sprintf("The realm %s names no sandbox stack; skipped, but its service images are pulled.", pack.Name))
		case pack.Sandbox == nil && all:
			skipped = append(skipped, fmt.Sprintf("The realm %s names no sandbox stack; skipped.", pack.Name))
		case pack.Sandbox == nil:
			return nil, nil, nil, fmt.Errorf("the realm %s names no sandbox stack in its pack.yaml", pack.Name)
		default:
			sb := pack.Sandbox
			ref, err := pack.StackRef(dir)
			if err != nil {
				return nil, nil, nil, err
			}
			want := []stackBuild{{ref: ref, target: sb.Target}}
			if sb.Build != "" {
				want[0].dir = filepath.Join(dir, sb.Build)
			}
			if sb.Acceptance != nil {
				acc, err := pack.AcceptanceRef(dir)
				if err != nil {
					return nil, nil, nil, err
				}
				a := stackBuild{ref: acc, target: sb.Acceptance.Target}
				if sb.Acceptance.Build != "" {
					a.dir = filepath.Join(dir, sb.Acceptance.Build)
				}
				want = append(want, a)
			}
			for _, w := range want {
				if !slices.ContainsFunc(stacks, func(x stackBuild) bool { return x.ref == w.ref }) {
					stacks = append(stacks, w)
				}
			}
		}
		if len(pack.Needs) > 0 && !all { // its providers are the project's runtime realm's services, which this run does not pull
			skipped = append(skipped, fmt.Sprintf("The realm %s needs %s, which the project's runtime realm provides: its service images are not pulled here; name the runtime realm with --realm too, or use --all.",
				pack.Name, strings.Join(pack.Needs, ", ")))
		}
		for _, v := range pack.ServiceList() {
			if !slices.Contains(services, v.Image) {
				services = append(services, v.Image)
			}
		}
	}
	return stacks, services, skipped, nil
}
