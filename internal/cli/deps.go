package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/deps"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/services"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// newDepsSandbox starts the sandbox driver of a bump. Tests replace it with a fake.
var newDepsSandbox = func(engine string, cfg sandbox.Config, work string) (gungnir.Sandbox, error) {
	return sandbox.New(engine, cfg, work)
}

// deps runs ygg deps bump (spec §9.6): one package to one version in a canary product, and only if the
// canary's deps and unit rungs pass, in each rollout product, one at a time. A product that fails is put
// back as it was and the bump stops. The change stays in the working copies, uncommitted: a human, or
// a run, commits it. Each product's own realm decides its checks; no policy file is read or written here.
func (c *command) deps(args []string) int {
	if len(args) == 0 || args[0] != "bump" {
		fmt.Fprint(c.err, usage)
		return ExitRefused
	}
	f, pos, ok := c.parse("deps bump", args[1:], 2, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "factory.yaml", "the factory config")
		fs.StringVar(&f.canary, "canary", "", "the product that takes the bump first: a leaf of the project: a path from the project, or absolute")
		fs.Func("rollout", "a product that takes the bump after the canary passes: a leaf of the project; repeat it for more", func(v string) error {
			f.rollout = append(f.rollout, v)
			return nil
		})
		fs.BoolVar(&f.host, "host", false, "run on the host, with no sandbox, when factory.yaml has no sandbox section (it has no effect with one)")
	})
	if !ok {
		return ExitRefused
	}
	if f.canary == "" {
		return c.refuse(errors.New("deps bump needs --canary"))
	}
	cfg, err := schema.LoadFactory(f.config)
	if err != nil {
		return c.refuse(err)
	}
	if cfg.Sandbox == nil && !f.host { // before the lock, the folder and the ledger: a refusal leaves nothing
		return c.refuse(errors.New("deps bump has no sandbox: factory.yaml has no sandbox section, so the package manager and its scripts would run on the host with the full network and your user id. Add a sandbox section, or pass --host to accept that"))
	}
	root, projectPath, err := tree.FindRoot(c.ctx, f.project)
	if err != nil {
		return c.refuse(err)
	}
	nodes, err := tree.Resolve(c.ctx, root, projectPath)
	if err != nil {
		return c.refuse(err)
	}
	project := nodes[len(nodes)-1]
	if project.Umbrella == nil || project.Kind != schema.ChildUmbrella {
		return c.refuse(fmt.Errorf("%s is not a project umbrella", f.project))
	}
	release, err := lock.Take(run.RunsLock(project.Dir))
	if errors.Is(err, lock.ErrHeld) {
		return c.refuse(run.ErrProjectBusy)
	} else if err != nil {
		return c.refuse(err)
	}
	defer release()
	id, err := run.NewIDNow()
	if err != nil {
		return c.refuse(err)
	}
	work := filepath.Join(project.Dir, ".factory", "deps", id)
	opener := &depsOpener{id: id, home: filepath.Join(work, "home"), places: map[string]depsPlace{}}
	var products []deps.Product
	var firstStack string
	for i, dir := range append([]string{f.canary}, f.rollout...) {
		p, place, err := c.depsProduct(cfg, root, projectPath, project, dir)
		if err != nil {
			return c.refuse(err)
		}
		if i == 0 {
			firstStack = place.realm.Stack
		}
		opener.places[p.Dir] = place
		products = append(products, p)
	}
	if cfg.Sandbox != nil {
		uid, gid := os.Getuid(), os.Getgid()
		if uid == 0 {
			return c.refuse(errors.New("ygg must not run as root: the sandbox's user is the operator's own"))
		}
		sc := sandbox.Config{Images: sandbox.ImagesFor(firstStack), UID: uid, GID: gid}
		for _, m := range cfg.Sandbox.Mounts {
			sc.Mounts = append(sc.Mounts, sandbox.PathMap{Host: m.Host, Engine: m.Engine})
		}
		// The driver starts with the first product, after every refusal of the bump: a refusal leaves no folder.
		opener.start = func() (gungnir.Sandbox, error) {
			return newDepsSandbox(cfg.Sandbox.Engine, sc, filepath.Join(work, "sandbox"))
		}
	}
	// The bump's folder, and its ledger, appear when the first line is written: a refusal leaves nothing.
	var l *ledger.Ledger
	defer func() {
		if l != nil {
			l.Close()
		}
	}()
	// The leaf's guard also runs the bump: its .git file is the one git made, and no repository is planted below it.
	guard := func(p deps.Product) error {
		l, err := leaf.Open(opener.places[p.Dir].leaf)
		if err != nil {
			return err
		}
		return l.Guard()
	}
	b := &deps.Bumper{Open: opener, Guard: guard, LogDir: filepath.Join(work, "logs"), Log: func(e gungnir.Event) error {
		if l == nil {
			if err := os.MkdirAll(work, 0o755); err != nil {
				return err
			}
			var err error
			if l, err = ledger.Open(filepath.Join(work, "ledger.jsonl"), ledger.Anchor{}); err != nil {
				return err
			}
			if cfg.Sandbox == nil { // --host: the first line says so, before any product is touched
				if err := l.Append(gungnir.Event{Name: gungnir.EventDepsBump, Run: id, Note: deps.HostNote}); err != nil {
					return err
				}
			}
		}
		e.Run = id
		return l.Append(e)
	}}
	rep, err := b.Bump(c.ctx, pos[0], pos[1], products[0], products[1:])
	var refusal *deps.Refusal
	if errors.As(err, &refusal) {
		return c.refuse(err)
	}
	for _, s := range rep.Steps {
		line := fmt.Sprintf("%s (%s): %s to %s: %s", s.Product, s.Stage, s.From, s.To, s.Verdict)
		if s.Reason != "" {
			line += " (" + s.Reason + ")"
		}
		fmt.Fprintln(c.out, line)
	}
	switch {
	case err != nil:
		fmt.Fprintf(c.err, "ygg: the bump %s stopped: %v\n", id, err)
		return ExitFailed
	case rep.Failed():
		last := rep.Steps[len(rep.Steps)-1]
		fmt.Fprintf(c.err, "ygg: %s failed (%s) and is back as it was; no product after it was touched. The ledger is %s\n", last.Product, last.Reason, filepath.Join(work, "ledger.jsonl"))
		return ExitFailed
	}
	fmt.Fprintf(c.out, "Bump %s done: %s is at %s in %d product(s). The change is in the working copies, uncommitted.\n", id, pos[0], pos[1], len(rep.Steps))
	return ExitDone
}

// depsPlace is where a product is in the tree, and the realm that provides it.
type depsPlace struct {
	parent, leaf tree.Node
	realm        *realm.Realm
	services     []gungnir.Service // what services.For gives the leaf; none when factory.yaml has no sandbox section
}

// depsProduct finds the leaf dir names in the project, the realm the project pins for it, and, when factory.yaml
// has a sandbox section, the services its sandbox starts: the same services.For gives a run's sandbox, from the
// realms the umbrella pins. A need with no provider is an error here, before any sandbox starts.
func (c *command) depsProduct(cfg *schema.Factory, root, projectPath string, project tree.Node, dir string) (deps.Product, depsPlace, error) {
	abs := dir
	if !filepath.IsAbs(dir) { // a relative path is from the project, so repos/api works from anywhere
		abs = filepath.Join(project.Dir, dir)
	}
	rel, err := filepath.Rel(project.Dir, filepath.Clean(abs))
	if err != nil || !filepath.IsLocal(rel) {
		return deps.Product{}, depsPlace{}, fmt.Errorf("%s is not inside the project %s", dir, project.Dir)
	}
	rel = filepath.ToSlash(rel)
	child, ok := project.Umbrella.Children[rel]
	if !ok || child.Kind != schema.ChildLeaf {
		return deps.Product{}, depsPlace{}, fmt.Errorf("%s is not a leaf of the project: --canary and --rollout name a leaf by its path, such as repos/api", dir)
	}
	name := child.Realm // a leaf's realm is its own pin; the project's graph is no realm
	if name == "" || cfg.Realms == nil {
		return deps.Product{}, depsPlace{}, fmt.Errorf("%s pins no realm, or factory.yaml has no realms section: the bump cannot tell which checks to run", rel)
	}
	store := &realm.Store{Dir: cfg.Realms.Dir, Kind: "realm"}
	rdir, err := store.Find(name)
	if err != nil {
		return deps.Product{}, depsPlace{}, err
	}
	r, err := realm.Load(rdir)
	if err != nil {
		return deps.Product{}, depsPlace{}, err
	}
	ns, err := tree.Resolve(c.ctx, root, filepath.Join(projectPath, rel))
	if err != nil {
		return deps.Product{}, depsPlace{}, err
	}
	// git is the way back from a bump that dies half way, so the manifest and the lock file start committed.
	leaf := ns[len(ns)-1]
	repo := git.Repo{GitDir: leaf.GitDir, WorkTree: leaf.Dir}
	dirty, err := repo.Run(c.ctx, append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, append(deps.Go.Files(), deps.NPM.Files()...)...)...)
	if err != nil {
		return deps.Product{}, depsPlace{}, err
	}
	if dirty != "" {
		return deps.Product{}, depsPlace{}, fmt.Errorf("%s has uncommitted changes in its manifest or lock file; commit or stash them first, so that git is the way back:\n%s", rel, dirty)
	}
	var svcs []gungnir.Service
	if cfg.Sandbox != nil {
		if svcs, err = services.For(project.Umbrella, []string{rel}, depsLoader{u: project.Umbrella, store: store, leaf: r, repo: rel}); err != nil {
			return deps.Product{}, depsPlace{}, fmt.Errorf("%s: %w", rel, err)
		}
	}
	p := deps.Product{Name: rel, Dir: ns[len(ns)-1].Dir, Realm: r.Name, Checks: r.Pack.Checks}
	return p, depsPlace{parent: ns[len(ns)-2], leaf: ns[len(ns)-1], realm: r, services: svcs}, nil
}

// depsLoader is the services.Loader of ygg deps: the realms the project's umbrella pins, found in the realm
// store. leaf is the realm already loaded for the product at repo.
type depsLoader struct {
	u     *schema.Umbrella
	store *realm.Store
	leaf  *realm.Realm
	repo  string
}

func (l depsLoader) Leaf(repo string) (*realm.Realm, error) {
	c := l.u.Children[repo]
	if repo == l.repo {
		return l.checked(l.leaf, repo, c.RealmVersion)
	}
	return l.load(c.Realm, c.RealmVersion, repo, "the leaf "+repo)
}

func (l depsLoader) Runtime() (*realm.Realm, error) {
	if l.u.Runtime == "" {
		return nil, nil
	}
	return l.load(l.u.Runtime, l.u.RuntimeVersion, "runtime", "the project")
}

func (l depsLoader) load(name, version, where, whose string) (*realm.Realm, error) {
	if name == "" {
		return nil, fmt.Errorf("%s pins no realm", whose)
	}
	dir, err := l.store.Find(name)
	if err != nil {
		return nil, err
	}
	r, err := realm.Load(dir)
	if err != nil {
		return nil, err
	}
	return l.checked(r, where, version)
}

// checked refuses a realm of another version than the pin at where, as a run does.
func (l depsLoader) checked(r *realm.Realm, where, version string) (*realm.Realm, error) {
	if version != "" && r.Version != version {
		return nil, fmt.Errorf("the realm store holds %s %s, but %s pins %s", r.Name, r.Version, where, version)
	}
	return r, nil
}

// depsOpener runs a product's commands in its sandbox, or on the host when factory.yaml has no sandbox.
type depsOpener struct {
	sb     gungnir.Sandbox
	start  func() (gungnir.Sandbox, error) // starts the driver of the sandbox; nil when factory.yaml has no sandbox
	id     string
	home   string
	places map[string]depsPlace // by the product's folder
}

func (o *depsOpener) Open(ctx context.Context, p deps.Product) (gungnir.Runner, func(context.Context) error, error) {
	pl := o.places[p.Dir]
	if o.sb == nil && o.start != nil {
		sb, err := o.start()
		if err != nil {
			return nil, nil, err
		}
		o.sb = sb
	}
	if o.sb == nil {
		return hostExec{dir: p.Dir, home: o.home}, func(context.Context) error { return nil }, nil
	}
	if pl.realm.Pack.Sandbox == nil {
		return nil, nil, fmt.Errorf("the realm %s of %s names no sandbox stack in its pack.yaml, and factory.yaml has a sandbox section", pl.realm.Name, p.Name)
	}
	env, err := o.sb.Up(ctx, gungnir.SandboxSpec{Run: o.id, Name: p.Name, Parent: pl.parent.Dir, Leaf: pl.leaf.Dir, GitDir: pl.leaf.GitDir,
		Egress: pl.realm.Pack.Sandbox.Egress, Stack: pl.realm.Stack, Services: pl.services})
	if err != nil {
		return nil, nil, err
	}
	return gungnir.Bind(o.sb, env), func(ctx context.Context) error { return o.sb.Down(ctx, env) }, nil
}

// hostExec runs a command on the host, in the product, in a process group of its own, with PATH, HOME and
// LANG only. It is the runner of a project whose factory.yaml has no sandbox section.
type hostExec struct{ dir, home string }

func (h hostExec) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	if err := c.Validate(); err != nil {
		return gungnir.ExecResult{}, err
	}
	if len(c.Env) > 0 {
		return gungnir.ExecResult{}, errors.New("a command on the host takes no environment of its own: credentials belong to the sandbox")
	}
	if err := os.MkdirAll(h.home, 0o755); err != nil {
		return gungnir.ExecResult{}, err
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = filepath.Join(h.dir, c.Dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return gungnir.ExecResult{}, ctx.Err()
	case err == nil:
		return gungnir.ExecResult{}, nil
	case errors.As(err, &ee):
		return gungnir.ExecResult{ExitCode: ee.ExitCode()}, nil
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission):
		fmt.Fprintf(c.Stderr, "ygg: %v\n", err) // a program that can't start is a command that failed, as in a shell
		return gungnir.ExecResult{ExitCode: 127}, nil
	}
	return gungnir.ExecResult{}, err
}
