package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/run"
)

// limitAgent stands in for an adapter that stops a session over its max_tokens: it records the Task of
// every session, and ends the first stop[node] sessions of a node as failed with token-limit, in the form
// of a failed result, or of an error when asError is set, without running the scripted agent.
type limitAgent struct {
	gungnir.Agent
	stop    map[string]int
	asError bool
	tasks   *[]gungnir.Task
}

func (l limitAgent) Run(ctx context.Context, t gungnir.Task) (gungnir.Result, error) {
	*l.tasks = append(*l.tasks, t)
	if l.stop[t.Node] > 0 {
		l.stop[t.Node]--
		spent := gungnir.Usage{InTok: 700, OutTok: 80, Turns: 2}
		if l.asError {
			return gungnir.Result{}, &gungnir.SessionError{Kind: gungnir.FailureTokenLimit, Err: errors.New("the session passed its limit of 500 tokens"), Usage: spent}
		}
		return gungnir.Result{Status: gungnir.StatusFailed, Failure: gungnir.FailureTokenLimit, Summary: "the session passed its limit of 500 tokens", Usage: spent}, nil
	}
	return l.Agent.Run(ctx, t)
}

func limited(r *run.Run, stop map[string]int, asError bool) *[]gungnir.Task {
	tasks := &[]gungnir.Task{}
	r.Setup.Agents["planner"] = limitAgent{Agent: r.Setup.Agents["planner"], stop: stop, asError: asError, tasks: tasks}
	return tasks
}

func tasksOf(tasks []gungnir.Task, node string) []gungnir.Task {
	var got []gungnir.Task
	for _, t := range tasks {
		if t.Node == node {
			got = append(got, t)
		}
	}
	return got
}

func TestALoopSessionOverItsTokenLimitIsOneAttemptAndTheLoopGoesOn(t *testing.T) {
	// The scripted agent counts sessions: the stopped first one never reaches it, so step 1 is a placeholder.
	script := "steps:\n  build:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, summary: built it, output: '{\"summary\": \"done\"}' }\n"
	r := startLoop(t, "unattended", script)
	tasks := limited(r, map[string]int{"build": 1}, false)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done after a later attempt passes", out, err)
	}
	if got := r.State.Loops["build"].State.Attempt; got != 2 {
		t.Errorf("the loop is in attempt %d; the stopped session was one attempt", got)
	}
	if n := len(tasksOf(*tasks, "build")); n != 2 {
		t.Errorf("the implementer ran %d sessions, want 2: the stopped one is not run again for free", n)
	}
	if text := ledgerText(t, r); strings.Contains(text, `"event":"retry"`) {
		t.Errorf("the stopped session was retried as a transient failure:\n%s", text)
	}
	if second := transcript(t, r, "build-2"); !strings.Contains(second, "the session passed its limit of 500 tokens") {
		t.Errorf("attempt 2 wasn't told why attempt 1 failed:\n%s", second)
	}
}

func TestAnAgentNodeOverItsTokenLimitFailsAndIsNotRetried(t *testing.T) {
	script := "steps:\n  a:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	w := &waits{}
	r := startWith(t, setup{nodes: oneNode, script: script, hooks: hooks(w)})
	tasks := limited(r, map[string]int{"a": 5}, true)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusFailed || out.Err == nil || !strings.Contains(out.Err.Error(), "500 tokens") {
		t.Fatalf("Advance() = %+v, %v; want a failed node that names the limit", out, err)
	}
	if n := len(*tasks); n != 1 || len(w.got) != 0 || r.State.Attempts["a"] != 1 {
		t.Errorf("%d call(s), %d wait(s), %d attempt(s); want exactly one call and no retry", n, len(w.got), r.State.Attempts["a"])
	}
	if r.State.PendingGate != "" {
		t.Errorf("pending gate = %q; a token limit is never a hard stop", r.State.PendingGate)
	}
}

func TestATokenLimitedSessionsUsageCountsInTheRunsTotals(t *testing.T) {
	r := startWith(t, setup{nodes: oneNode, script: "steps:\n  a:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"})
	limited(r, map[string]int{"a": 1}, false)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want a failed node", out, err)
	}
	if u := r.State.Usage; u.InTok != 700 || u.OutTok != 80 || u.Turns != 2 {
		t.Errorf("usage = %+v; the stopped session's tokens count against the caps", u)
	}
	// The same, for a session that ends with an error that carries its usage.
	r = startWith(t, setup{nodes: oneNode, script: "steps:\n  a:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"})
	limited(r, map[string]int{"a": 1}, true)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusFailed {
		t.Fatalf("Advance() = %+v, %v; want a failed node", out, err)
	}
	if u := r.State.Usage; u.InTok != 700 || u.OutTok != 80 || u.Turns != 2 {
		t.Errorf("usage = %+v after an error; the stopped session's tokens count against the caps", u)
	}
}

func TestMaxTokensReachesTheTaskOfAnAgentNode(t *testing.T) {
	nodes := "  - { id: a, kind: agent, role: planner, prompt: p.md, repo: repos/app, max_tokens: 1234, input: [rejection], output: ygg.implement/v1 }\n  - { id: b, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n"
	script := "steps:\n  a:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n  b:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{nodes: nodes, script: script})
	tasks := limited(r, nil, false)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if a, b := tasksOf(*tasks, "a"), tasksOf(*tasks, "b"); len(a) != 1 || a[0].MaxTokens != 1234 || len(b) != 1 || b[0].MaxTokens != 0 {
		t.Errorf("tasks a = %+v, b = %+v; want MaxTokens 1234 and 0 (no limit)", a, b)
	}
}

func TestMaxTokensReachesTheTasksOfALoopsImplementerAndReviewer(t *testing.T) {
	nodes := strings.Replace(reviewLoop, "    attempts: 3\n", "    attempts: 3\n    max_tokens: 2000\n", 1)
	nodes = strings.Replace(nodes, "prompt: p.md, output: ygg.review/v1 }\n", "prompt: p.md, output: ygg.review/v1, max_tokens: 900 }\n", 1)
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "key-points", pack: unitCheck, nodes: nodes, script: script})
	tasks := limited(r, nil, false)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if b, s := tasksOf(*tasks, "build"), tasksOf(*tasks, "build-spec"); len(b) != 1 || b[0].MaxTokens != 2000 || len(s) != 1 || s[0].MaxTokens != 900 {
		t.Errorf("implementer tasks %+v, reviewer tasks %+v; want MaxTokens 2000 and 900", b, s)
	}
}

func TestAReviewerSessionOverItsTokenLimitIsAskedOnceMoreAndNeverAHardStop(t *testing.T) {
	nodes := strings.Replace(reviewLoop, "prompt: p.md }\n", "prompt: p.md, max_tokens: 900 }\n", 1)
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "key-points", pack: unitCheck, nodes: nodes, script: script})
	tasks := limited(r, map[string]int{"build-spec": 1}, false)
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v; want done: the reviewer answers at the second ask", out, err)
	}
	if n := len(tasksOf(*tasks, "build-spec")); n != 2 {
		t.Errorf("the reviewer ran %d sessions, want 2 (the stopped one, and the second ask)", n)
	}
}
