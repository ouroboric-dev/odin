package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/bridge"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/metrics"
	"github.com/ouroboric-dev/odin/internal/run"
)

// spendEvery is how often the bridge tells the adapter what the run has cost so far.
var spendEvery = 30 * time.Second

// bridge runs ygg bridge: the factory on a Harbor task (bench/harbor). The adapter starts it and sends the brief
// on standard input; it runs the graph in a scratch project, hands the committed product to the adapter, and says
// done. Standard output is the bridge and nothing else. A run that stops at a gate is handed over as it is, for no
// one can approve in a Harbor run. The exit code is the run's, or 2 when nothing could start.
func (c *command) bridge(args []string) int {
	var work string
	f, _, ok := c.parse("bridge", args, 0, func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.config, "config", "", "the factory config")
		fs.StringVar(&f.graph, "graph", "", "the graph pack: a name or a folder")
		fs.StringVar(&work, "work", "", "a folder to work in, which must not exist yet (default: a temporary one, removed at the end)")
	})
	if !ok {
		return ExitRefused
	}
	if f.set["project"] {
		return c.refuse(errors.New("bridge makes its own project: --project is not accepted (use --work)"))
	}
	if f.config == "" || f.graph == "" {
		return c.refuse(errors.New("bridge needs --config and --graph"))
	}
	cfg, err := schema.LoadFactory(f.config)
	if err != nil {
		return c.refuse(err)
	}
	conn := bridge.NewConn(c.in, c.out)
	defer conn.Close()
	// A signal closes the bridge for the whole command, so that no call waits for an adapter that does not answer.
	defer context.AfterFunc(c.ctx, conn.Close)()
	start, err := conn.ReadStart()
	if err != nil {
		return c.refuse(err)
	}
	if err := checkTaskBudget(cfg, start.BudgetUSD); err != nil {
		return c.refuse(err)
	}
	if work == "" {
		if work, err = os.MkdirTemp("", "ygg-bridge-"); err != nil {
			return c.refuse(err)
		}
		defer os.RemoveAll(work)
	} else if err := os.Mkdir(work, 0o755); err != nil {
		return c.refuse(fmt.Errorf("--work must be a folder that does not exist yet: %w", err))
	}
	fmt.Fprintf(c.err, "bridge: working in %s\n", work)
	project, brief := filepath.Join(work, "project"), filepath.Join(work, "brief.md")
	if err := newProject(c.ctx, project); err != nil {
		return c.refuse(err)
	}
	if err := os.WriteFile(brief, []byte(start.Instruction), 0o644); err != nil {
		return c.refuse(err)
	}

	// The run stops when the adapter goes away (the bridge is dead), when a spend report fails, and on a signal.
	runCtx, cancelRun := context.WithCancel(c.ctx)
	defer cancelRun()
	go func() {
		select {
		case <-conn.Dead():
			cancelRun()
		case <-runCtx.Done():
		}
	}()
	// The run prints to standard error: standard output is the bridge.
	sub := &command{ctx: runCtx, out: c.err, err: c.err}
	stopSpend := c.reportSpend(conn, project, cancelRun)
	runArgs := []string{"--project", project, "--config", f.config, "--graph", f.graph, "--brief", brief}
	if f.mode != "" {
		runArgs = append(runArgs, "--mode", f.mode)
	}
	code := sub.run(runArgs)
	stopSpend()
	fmt.Fprintf(c.err, "bridge: the run ended with exit code %d\n", code)
	if c.ctx.Err() != nil {
		fmt.Fprintln(c.err, "bridge: stopped by a signal: nothing is handed over")
		return stoppedCode(code)
	}
	select {
	case <-conn.Dead():
		fmt.Fprintf(c.err, "bridge: the adapter went away (%v): nothing is handed over\n", conn.Err())
		return stoppedCode(code)
	default:
	}
	if code == ExitRefused {
		return code // nothing started: no product to hand over, and the adapter fails the run for want of done
	}

	state := lastState(project)
	if state != nil {
		if state.Pending != "" {
			fmt.Fprintf(c.err, "bridge: the run stopped at %s (%s): there is no one to approve in a Harbor run, so what exists is handed over\n", state.Pending, state.PendingGate)
		}
		if err := conn.Spend(spentUSD(state)); err != nil {
			fmt.Fprintf(c.err, "bridge: %v\n", err)
		}
	}
	export := filepath.Join(work, "export")
	if err := c.exportProduct(project, export); err != nil {
		fmt.Fprintf(c.err, "bridge: %v\n", err)
		code = max(code, ExitFailed)
	} else if err := conn.UploadDir(export, start.Workdir); err != nil {
		fmt.Fprintf(c.err, "bridge: %v\n", err)
		code = max(code, ExitFailed)
	}
	if ch, err := conn.Selfcheck(); err != nil {
		fmt.Fprintf(c.err, "bridge: %v\n", err)
	} else {
		fmt.Fprintf(c.err, "bridge: the adapter's self-check: passed=%v %s\n", ch.Passed, metrics.Clean(ch.Detail))
	}
	ch, err := conn.Done()
	if err != nil {
		fmt.Fprintf(c.err, "bridge: %v\n", err)
		return max(code, ExitFailed)
	}
	fmt.Fprintf(c.err, "bridge: done; the adapter's final self-check: passed=%v %s\n", ch.Passed, metrics.Clean(ch.Detail))
	return code
}

// checkTaskBudget refuses a config whose cap for one run is above the budget the task gives, when a role pays
// per token. A subscription's dollar figure is what the run would cost on the API: it is no cap.
func checkTaskBudget(cfg *schema.Factory, budget float64) error {
	if cfg.PaysPerToken() && cfg.Budget.PerRun.USD > budget {
		return fmt.Errorf("the config lets one run spend $%v and the task's budget is $%v; lower budget.per_run.usd", cfg.Budget.PerRun.USD, budget)
	}
	return nil
}

// stoppedCode is the exit code of a run the bridge stopped: the run's own when it says the run did not end well,
// else a failure (a run that gave up tasks is not a success when nothing was handed over). A run that was cancelled before it began says "refused", but the bridge did not refuse it.
func stoppedCode(code int) int {
	if code == ExitDone || code == ExitRefused || code == ExitBlocked {
		return ExitFailed
	}
	return code
}

// spentUSD is what the run has spent: the dollars a paid adapter billed. The caps count it, and not the cost figure,
// which for a subscription is only what the sessions would cost on the API.
func spentUSD(s *run.State) float64 { return s.Usage.BilledUSD }

// reportSpend starts a goroutine that sends the run's cost so far every spendEvery. The returned function stops
// it and waits, so no request is in flight when the caller sends the next one.
func (c *command) reportSpend(conn *bridge.Conn, project string, onFail func()) (stop func()) {
	ctx, cancel := context.WithCancel(c.ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(spendEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if s := lastState(project); s != nil {
					if err := conn.Spend(spentUSD(s)); err != nil {
						fmt.Fprintf(c.err, "bridge: %v\n", err)
						onFail() // a spend the adapter does not take: no one is counting, so the run stops
						return
					}
				}
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}

// lastState is the state of the project's newest run, or nil when there is none.
func lastState(project string) *run.State {
	entries, err := os.ReadDir(filepath.Join(project, ".factory", "runs"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && run.ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	s, err := run.LoadState(run.Dir(project, slices.Max(ids)))
	if err != nil {
		return nil
	}
	return s
}

// exportProduct writes the committed files of the project and its leaves to dst, as the grader's /app is made.
func (c *command) exportProduct(project, dst string) error {
	s := lastState(project)
	if s == nil {
		return errors.New("the run left no record to export a product from")
	}
	r, err := run.Open(c.ctx, project, s.ID, "")
	if err != nil {
		return err
	}
	defer r.Close()
	return r.ExportApp(c.ctx, dst)
}

// newProject makes an empty project: a git repository whose umbrella.yaml sets root: true, as a person does.
func newProject(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := git.Run(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, schema.UmbrellaFile), []byte("root: true\nowner: ouroboric-dev\nvisibility: private\n"), 0o644); err != nil {
		return err
	}
	if _, err := git.Run(ctx, dir, "add", "--", schema.UmbrellaFile); err != nil {
		return err
	}
	_, err := git.Run(ctx, dir, "-c", "user.name=ygg", "-c", "user.email=ygg@example.invalid", "commit", "-q", "-m", "A Harbor task's project")
	return err
}
