package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir/schema"
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
		fs.StringVar(&f.realm, "realm", "", "the realm directory (holds pack.yaml): its sandbox stack is what gets built")
	})
	if !ok {
		return ExitRefused
	}
	if f.realm == "" {
		return c.refuse(fmt.Errorf("sandbox build needs --realm"))
	}
	cfg, err := schema.LoadFactory(f.config)
	if err != nil {
		return c.refuse(err)
	}
	if cfg.Sandbox == nil {
		return c.refuse(fmt.Errorf("%s has no sandbox section", f.config))
	}
	pack, err := schema.LoadPack(f.realm)
	if err != nil {
		return c.refuse(err)
	}
	if pack.Sandbox == nil {
		return c.refuse(fmt.Errorf("the realm %s names no sandbox stack in its pack.yaml", pack.Name))
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
	fmt.Fprintf(c.out, "Pulling and building the images for %s; this takes a few minutes.\n", pack.Sandbox.Stack)
	imgs, err := d.Build(c.ctx, pack.Sandbox.Stack)
	if err != nil {
		fmt.Fprintf(c.err, "ygg: the build failed: %v\n", err)
		return ExitFailed
	}
	fmt.Fprintf(c.out, "Built:\n  %s\n  %s\n", imgs.Work, imgs.Firewall)
	return ExitDone
}
