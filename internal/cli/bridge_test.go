package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// harbor plays the Harbor agent adapter for `ygg bridge`: it sends the start line, answers each request, and
// copies a folder the factory uploads into its own, as the adapter copies it into the task environment.
type harbor struct {
	stdout   []string // every line the factory wrote to standard output
	requests []map[string]any
	uploaded string // where the uploaded folder was copied
	code     int
	stderr   string
}

func runBridge(t *testing.T, start string, args ...string) *harbor {
	t.Helper()
	return runBridgeWithDetail(t, "PASS port 3000 answered", start, args...)
}

// runBridgeWithDetail is runBridge with the detail the adapter's self-check answers.
func runBridgeWithDetail(t *testing.T, detail, start string, args ...string) *harbor {
	t.Helper()
	h := &harbor{uploaded: t.TempDir()}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var errBuf strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.code = MainWithInput(context.Background(), append([]string{"bridge"}, args...), inR, outW, &errBuf)
		outW.Close()
	}()
	// A bridge that refuses before it reads its input never takes the start line, and a pipe write waits.
	go fmt.Fprintln(inW, start) //nolint:errcheck // the pipe closes when the test ends
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		h.stdout = append(h.stdout, line)
		var req map[string]any
		if json.Unmarshal([]byte(line), &req) != nil {
			continue // agent.py would answer with an error; the test only needs to see the line
		}
		h.requests = append(h.requests, req)
		reply := map[string]any{"id": req["id"], "ok": true}
		switch req["op"] {
		case "upload_dir":
			if err := os.CopyFS(h.uploaded, os.DirFS(req["source"].(string))); err != nil {
				reply["ok"], reply["error"] = false, err.Error()
			}
		case "selfcheck", "done":
			reply["passed"], reply["detail"] = true, detail
		}
		b, _ := json.Marshal(reply)
		fmt.Fprintln(inW, string(b))
		if req["op"] == "done" {
			break
		}
	}
	inW.Close()
	wg.Wait()
	inR.Close()
	h.stderr = errBuf.String()
	return h
}

func (h *harbor) ops() []string {
	var ops []string
	for _, r := range h.requests {
		// An upload shows where it goes ("upload_dir/app"): that is part of what the test checks.
		ops = append(ops, r["op"].(string)+strings.TrimPrefix(fmt.Sprint(r["target"]), "<nil>"))
	}
	return ops
}

func bridgeStart(budget string) string {
	return `{"type":"start","instruction":"Build the toy product.\n","workdir":"/app","budget_usd":` + budget + `}`
}

func TestBridgeRunsTheFactoryOnTheBriefAndUploadsTheCommittedProductThenSaysDone(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	work := filepath.Join(t.TempDir(), "work")
	h := runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--work", work)
	if h.code != ExitDone {
		t.Fatalf("exit %d, stderr:\n%s", h.code, h.stderr)
	}
	ops := h.ops()
	if got := strings.Join(ops, " "); !strings.HasSuffix(got, "spend upload_dir/app selfcheck done") {
		t.Errorf("requests = %s; want spend, upload_dir, selfcheck, done in the end", got)
	}
	if _, err := os.Stat(filepath.Join(h.uploaded, "repos", "app", "build.txt")); err != nil {
		t.Errorf("the committed product is not in the upload: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "brief.md")); !strings.HasPrefix(string(b), "Build the toy product.") {
		t.Errorf("the run's brief = %q", b)
	}
	if !strings.Contains(h.stderr, "the run ended with exit code 0") {
		t.Errorf("stderr lacks the run's outcome:\n%s", h.stderr)
	}
}

// Standard output is the bridge. A line that is not a request fails the adapter's run.
func TestBridgeWritesNothingToStandardOutputButRequests(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	h := runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--work", filepath.Join(t.TempDir(), "w"))
	if len(h.stdout) == 0 || len(h.stdout) != len(h.requests) {
		t.Fatalf("%d lines on standard output, %d of them requests:\n%s", len(h.stdout), len(h.requests), strings.Join(h.stdout, "\n"))
	}
	for _, r := range h.requests {
		if r["id"] == nil || r["op"] == nil {
			t.Errorf("a request without an id or an op: %v", r)
		}
	}
	if !strings.Contains(h.stderr, "run ") {
		t.Errorf("the factory's own output should go to standard error:\n%s", h.stderr)
	}
}

// Harbor has no one to approve a gate. The run stops, and the bridge still hands over what exists and ends.
func TestBridgeSendsWhatExistsAndDoneWhenTheRunStopsAtAGate(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: key-points\n")
	h := runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--work", filepath.Join(t.TempDir(), "w"))
	if h.code != ExitStopped {
		t.Fatalf("exit %d, want %d (the run stopped at the design gate):\n%s", h.code, ExitStopped, h.stderr)
	}
	if got := strings.Join(h.ops(), " "); !strings.HasSuffix(got, "upload_dir/app selfcheck done") {
		t.Errorf("requests = %s; the run is over for Harbor, so it must still end with done", got)
	}
	if !strings.Contains(h.stderr, "design-gate") || !strings.Contains(h.stderr, "no one to approve") {
		t.Errorf("stderr must say where and why it stopped:\n%s", h.stderr)
	}
}

func TestBridgeRefusesWhatItCannotRunBeforeItStartsAnything(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\n")
	for name, c := range map[string]struct {
		start string
		args  []string
	}{
		"no config":                 {bridgeStart("100"), []string{"--graph", toyRealm(t)}},
		"no graph":                  {bridgeStart("100"), []string{"--config", config}},
		"a bad start":               {`{"type":"stop"}`, []string{"--config", config, "--graph", toyRealm(t)}},
		"a graph that is not there": {bridgeStart("100"), []string{"--config", config, "--graph", "/no/such/graph"}},
		"a bad config":              {bridgeStart("100"), []string{"--config", "/no/such/factory.yaml", "--graph", toyRealm(t)}},
		"a work folder that exists": {bridgeStart("100"), []string{"--config", config, "--graph", toyRealm(t), "--work", os.TempDir()}},
	} {
		h := runBridge(t, c.start, c.args...)
		if h.code != ExitRefused || len(h.requests) != 0 || h.stderr == "" {
			t.Errorf("%s: exit %d, %d requests, stderr %q; want a refusal and no request", name, h.code, len(h.requests), h.stderr)
		}
	}
}

// The adapter stops the factory when spend passes its budget, but the factory should never plan to.
func TestACapOfThePaidRolesAboveTheTasksBudgetIsRefused(t *testing.T) {
	paid := &schema.Factory{Roles: map[string]schema.Role{"implementer": {Agent: "opencode", Model: "openrouter/x"}}, Budget: schema.Budget{PerRun: schema.Cap{USD: 50}}}
	if err := checkTaskBudget(paid, 10); err == nil || !strings.Contains(err.Error(), "50") {
		t.Errorf("checkTaskBudget(paid $50, $10) = %v; want a refusal that names the cap", err)
	}
	if err := checkTaskBudget(paid, 100); err != nil {
		t.Errorf("checkTaskBudget(paid $50, $100) = %v", err)
	}
	sub := &schema.Factory{Roles: map[string]schema.Role{"implementer": {Agent: "claude-code", Model: "sonnet"}}, Budget: schema.Budget{PerRun: schema.Cap{USD: 50}}}
	if err := checkTaskBudget(sub, 10); err != nil {
		t.Errorf("a subscription's dollar figure is not a cap: checkTaskBudget() = %v", err)
	}
}

// The dollar caps count what a paid adapter billed (plan decision 4); a subscription's figure is not spent money.
func TestBridgeReportsTheBilledDollarsAndNotTheCostFigure(t *testing.T) {
	s := &run.State{Usage: gungnir.Usage{CostUSD: 4.5, CostEstimated: true, BilledUSD: 0.5}}
	if got := spentUSD(s); got != 0.5 {
		t.Errorf("spentUSD = %v; want the billed 0.5, not the cost figure 4.5", got)
	}
}

func TestBridgeRefusesProject(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\n")
	h := runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--project", t.TempDir())
	if h.code != ExitRefused || len(h.requests) != 0 || !strings.Contains(h.stderr, "--project") {
		t.Errorf("exit %d, %d requests, stderr %q; want a refusal that names --project", h.code, len(h.requests), h.stderr)
	}
}

// bridgeAlone runs ygg bridge with the pipes in the test's hands and returns the exit code once it ends.
func bridgeAlone(t *testing.T, ctx context.Context, work string, args ...string) (inW *io.PipeWriter, outR *bufio.Reader, code chan int, stderr *strings.Builder) {
	t.Helper()
	inR, inW := io.Pipe()
	outPR, outW := io.Pipe()
	stderr = &strings.Builder{}
	code = make(chan int, 1)
	go func() { code <- MainWithInput(ctx, append([]string{"bridge"}, args...), inR, outW, stderr) }()
	t.Cleanup(func() { inW.Close(); outPR.Close() })
	return inW, bufio.NewReader(outPR), code, stderr
}

func waitCode(t *testing.T, code chan int) int {
	t.Helper()
	select {
	case c := <-code:
		return c
	case <-time.After(60 * time.Second):
		t.Fatal("ygg bridge does not end")
		return 0
	}
}

// An adapter that goes away mid-run leaves no one to hand the product to: the run stops, and nothing more is sent.
func TestBridgeStopsTheRunAndSendsNothingWhenTheAdapterClosesItsOutput(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	inW, outR, code, stderr := bridgeAlone(t, context.Background(), filepath.Join(t.TempDir(), "w"), "--config", config, "--graph", toyRealm(t))
	fmt.Fprintln(inW, bridgeStart("100"))
	inW.Close() // the adapter is gone
	c := waitCode(t, code)
	var sent []string
	for {
		line, err := outR.ReadString('\n')
		if err != nil {
			break
		}
		sent = append(sent, line)
	}
	if c == ExitDone || c == ExitRefused {
		t.Errorf("exit %d; want a failure that is not a clean end or a refusal:\n%s", c, stderr)
	}
	for _, l := range sent {
		if strings.Contains(l, `"upload_dir"`) || strings.Contains(l, `"done"`) || strings.Contains(l, `"selfcheck"`) {
			t.Errorf("sent %s after the adapter went away", l)
		}
	}
	if !strings.Contains(stderr.String(), "the adapter went away") {
		t.Errorf("stderr must say why nothing was handed over:\n%s", stderr)
	}
}

// An adapter that stops replying cannot be told from a slow one, but a signal must end the command all the same.
func TestBridgeEndsOnASignalWhileTheAdapterDoesNotReply(t *testing.T) {
	old := spendEvery
	spendEvery = 5 * time.Millisecond
	t.Cleanup(func() { spendEvery = old })
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inW, outR, code, stderr := bridgeAlone(t, ctx, filepath.Join(t.TempDir(), "w"), "--config", config, "--graph", toyRealm(t))
	fmt.Fprintln(inW, bridgeStart("100"))
	first, err := outR.ReadString('\n') // a request; it is never answered
	if err != nil || !strings.Contains(first, `"op"`) {
		t.Fatalf("no request: %q, %v", first, err)
	}
	cancel()
	if c := waitCode(t, code); c == ExitDone {
		t.Errorf("exit %d after the signal:\n%s", c, stderr)
	}
	if !strings.Contains(stderr.String(), "stopped by a signal") || strings.Contains(stderr.String(), "went away") {
		t.Errorf("stderr must say that a signal stopped the run, not that the adapter went away:\n%s", stderr)
	}
	rest, _ := io.ReadAll(outR)
	if strings.Contains(string(rest), `"done"`) {
		t.Errorf("sent done after the signal: %s", rest)
	}
}

// A refused spend stops the run, but the bridge lives on to hand over; a signal must still end it when the adapter
// then stops replying.
func TestBridgeEndsOnASignalAfterARefusedSpendWhenTheAdapterStopsReplying(t *testing.T) {
	old := spendEvery
	spendEvery = 5 * time.Millisecond
	t.Cleanup(func() { spendEvery = old })
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inW, outR, code, stderr := bridgeAlone(t, ctx, filepath.Join(t.TempDir(), "w"), "--config", config, "--graph", toyRealm(t))
	fmt.Fprintln(inW, bridgeStart("100"))
	first, err := outR.ReadString('\n')
	var req map[string]any
	if err != nil || json.Unmarshal([]byte(first), &req) != nil {
		t.Fatalf("no request: %q, %v", first, err)
	}
	reply, _ := json.Marshal(map[string]any{"id": req["id"], "ok": false, "error": "over budget"})
	fmt.Fprintln(inW, string(reply))
	second, err := outR.ReadString('\n') // the next request is never answered
	if err != nil || !strings.Contains(second, `"op"`) {
		t.Fatalf("no second request: %q, %v\n%s", second, err, stderr)
	}
	cancel()
	done := make(chan int, 1)
	go func() { done <- <-code }()
	select {
	case c := <-done:
		if c == ExitDone {
			t.Errorf("exit %d after the signal:\n%s", c, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ygg bridge still waits for the adapter 5 s after the signal")
	}
	rest, _ := io.ReadAll(outR)
	if strings.Contains(string(rest), `"done"`) {
		t.Errorf("sent done after the signal: %s", rest)
	}
}

func TestAStoppedBridgeNeverExitsAsDoneOrRefusedOrDoneWithGivenUpTasks(t *testing.T) {
	for code, want := range map[int]int{ExitDone: ExitFailed, ExitRefused: ExitFailed, ExitBlocked: ExitFailed, ExitFailed: ExitFailed, ExitStopped: ExitStopped, ExitPaused: ExitPaused} {
		if got := stoppedCode(code); got != want {
			t.Errorf("stoppedCode(%d) = %d; want %d", code, got, want)
		}
	}
}

// A Harbor teardown can SIGKILL the bridge: the folder it works in must be findable afterwards.
func TestBridgeNamesItsWorkFolderOnStandardErrorAtTheStart(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	work := filepath.Join(t.TempDir(), "work")
	h := runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--work", work)
	if !strings.Contains(h.stderr, "bridge: working in "+work+"\n") {
		t.Errorf("stderr does not name the work folder:\n%s", h.stderr)
	}
	for _, l := range h.stdout {
		if strings.Contains(l, "working in") {
			t.Errorf("standard output must hold only requests: %q", l)
		}
	}
	// With no --work, the temporary folder is named too.
	h = runBridge(t, bridgeStart("100"), "--config", config, "--graph", toyRealm(t))
	if !strings.Contains(h.stderr, "bridge: working in "+os.TempDir()) || !strings.Contains(h.stderr, "ygg-bridge-") {
		t.Errorf("stderr does not name the temporary work folder:\n%s", h.stderr)
	}
}

// The adapter's self-check detail is data from another program: no control character reaches the terminal.
func TestBridgeCleansTheAdaptersSelfCheckDetail(t *testing.T) {
	_, config := project(t, "script.yaml", "mode: unattended\ngates:\n  design: auto\n")
	h := runBridgeWithDetail(t, "bad\x1b[2J\u202eevil\nsecond", bridgeStart("100"), "--config", config, "--graph", toyRealm(t), "--work", filepath.Join(t.TempDir(), "w"))
	if !strings.Contains(h.stderr, "self-check: passed=true bad [2J evil second\n") {
		t.Errorf("the detail was not cleaned:\n%q", h.stderr)
	}
	if strings.ContainsAny(h.stderr, "\x1b\u202e") {
		t.Errorf("stderr holds a control character:\n%q", h.stderr)
	}
}
