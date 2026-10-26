package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir/schema"
)

// shellSafePath is what ygg models server prints unquoted for a path: nothing a shell reads as syntax.
var shellSafePath = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]*$`)

// models runs ygg models: the commands the umbrella's scripts/ygg-models.sh asks of the list of local models,
// so a shell never parses YAML (local-models spec §3.2, §3.3, §3.4).
//
//	ygg models preset FILE   llama-server's preset INI, on stdout
//	ygg models server FILE   the server's six settings as KEY=value lines
//	ygg models image         the tag of the model server's image, without building it
//	ygg models build         builds the image; the one command of a model that pulls and builds
func (c *command) models(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.err, usage)
		return ExitRefused
	}
	switch args[0] {
	case "preset", "server":
		_, pos, ok := c.parse("models "+args[0], args[1:], 1, nil)
		if !ok {
			return ExitRefused
		}
		data, err := os.ReadFile(pos[0])
		if err != nil {
			return c.refuse(err)
		}
		list, err := schema.ParseModels(pos[0], data)
		if err != nil {
			return c.refuse(err)
		}
		if args[0] == "preset" {
			c.out.Write(list.Preset())
			return ExitDone
		}
		// ParseModels checked each value: a name of a DNS label, ports in range, clean absolute paths. A shell reads
		// them unquoted, and a clean path may still hold a newline, a space or $(…): the two paths are refused unless
		// they are plain.
		s := list.Server
		for _, kv := range [][2]string{{"weights", s.Weights}, {"state", s.State}} {
			if !shellSafePath.MatchString(kv[1]) {
				return c.refuse(fmt.Errorf("%s: server %s %q has characters a shell could misread; use a path of letters, digits and . _ / @ + - only, without spaces or special characters (a symlink works)", pos[0], kv[0], kv[1]))
			}
		}
		fmt.Fprintf(c.out, "NAME=%s\nNETWORK=%s\nPORT=%d\nHOST_PORT=%d\nWEIGHTS=%s\nSTATE=%s\n", s.Name, s.Network, s.Port, s.HostPort, s.Weights, s.State)
		return ExitDone
	case "image":
		if _, _, ok := c.parse("models image", args[1:], 0, nil); !ok {
			return ExitRefused
		}
		fmt.Fprintln(c.out, sandbox.ModelsImage())
		return ExitDone
	case "build":
		return c.modelsBuild(args[1:])
	}
	fmt.Fprint(c.err, usage)
	return ExitRefused
}

func (c *command) modelsBuild(args []string) int {
	arch := ""
	f, _, ok := c.parse("models build", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
		fs.StringVar(&arch, "cuda-arch", "", "the CUDA architectures to build for, such as 89 or 86;89 (default 89)")
	})
	if !ok {
		return ExitRefused
	}
	cfg, err := schema.LoadFactory(f.config)
	if err != nil {
		return c.refuse(err)
	}
	if cfg.Sandbox == nil {
		return c.refuse(fmt.Errorf("%s has no sandbox section", f.config))
	}
	work, err := os.MkdirTemp("", "ygg-models-build-")
	if err != nil {
		return c.refuse(err)
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(cfg.Sandbox.Engine, sandbox.Config{}, work)
	if err != nil {
		return c.refuse(err)
	}
	tag, err := d.BuildModels(c.ctx, arch)
	if err != nil {
		return c.refuse(err)
	}
	fmt.Fprintln(c.out, tag)
	return ExitDone
}

// checkModels checks the local models the roles name (spec §3.9): the list holds each one (an error: a run would
// refuse), the server answers on the host (a warning: the operator starts it), and the model network is internal
// (a warning: the firewall's rules for the server assume it). A config with no local role adds no line.
func (c *command) checkModels(f *schema.Factory, report func(level, what, msg string)) {
	names := f.LocalModels()
	if len(names) == 0 {
		return
	}
	data, err := os.ReadFile(f.Models.File)
	if err != nil {
		report("ERROR", "models", err.Error())
		return
	}
	list, err := schema.ParseModels(f.Models.File, data)
	if err != nil {
		report("ERROR", "models", err.Error())
		return
	}
	ok := true
	for _, name := range names {
		if _, found := list.Models[name]; !found {
			report("ERROR", "models", fmt.Sprintf("a role uses local/%s, and %s does not list it", name, f.Models.File))
			ok = false
		}
	}
	if !ok {
		return
	}
	if err := c.serverAnswers(list.HostURL() + "/models"); err != nil {
		report("WARN", "models", fmt.Sprintf("the model server does not answer at %s (start it: scripts/ygg-models.sh start): %v", list.HostURL(), err))
		ok = false
	}
	if f.Sandbox != nil {
		if got, err := c.networkInternal(f.Sandbox.Engine, list.Server.Network); err != nil {
			report("WARN", "models", fmt.Sprintf("the model network %s can't be inspected: %v", list.Server.Network, err))
			ok = false
		} else if got != "true" {
			report("WARN", "models", fmt.Sprintf("the model network %s is not internal (podman network inspect says %q): it must have no route out; run scripts/ygg-models.sh start", list.Server.Network, got))
			ok = false
		}
	}
	if ok && len(names) > 1 {
		report("WARN", "models", fmt.Sprintf("the roles name %d local models, %s; a run may use only one, and refuses a graph that names more", len(names), strings.Join(slices.Sorted(slices.Values(names)), ", ")))
		ok = false
	}
	if ok {
		report("OK", "models", fmt.Sprintf("%s serves %s at %s", list.Server.Name, strings.Join(slices.Sorted(slices.Values(names)), ", "), list.HostURL()))
	}
}

func (c *command) serverAnswers(url string) error {
	ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /models answered %d", resp.StatusCode)
	}
	return nil
}

// networkInternal asks the engine whether the network is internal, in the driver's own environment, so it reaches the
// factory's engine and no other.
func (c *command) networkInternal(engine, network string) (string, error) {
	work, err := os.MkdirTemp("", "ygg-doctor-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	d, err := sandbox.New(engine, sandbox.Config{}, work)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Podman, "network", "inspect", "--format", "{{.Internal}}", network)
	cmd.Env = d.Env()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		if strings.Contains(msg, "not found") || strings.Contains(msg, "no such network") {
			msg += " (create it: scripts/ygg-models.sh start)"
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}
