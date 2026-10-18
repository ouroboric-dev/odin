package engine

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/gate"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/routing"
	"github.com/ouroboric-dev/odin/internal/run"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// contractDiffer compares two versions of a contract: the oasdiff on the host's PATH, or nil when there is
// none (the realm images bake oasdiff in; a host without it judges by the index alone). Tests replace it.
var contractDiffer = routing.HostOasdiff()

// maxContractChanges bounds the contract files one move is judged on; a move that changes more goes to a
// person without a verdict by file.
const maxContractChanges = 64

// maxIndexBytes is the largest impact index the gate reads from git (the same cap as routing.ReadImpact).
const maxIndexBytes = 8 << 20

// contractJudgement is the pointer-contract gate's check (spec §3.4, §8.2). The gate node judges the move
// of the next pointer node: the changes between the commit the project points at and the leaf's main, in
// the leaf's own git, against the impact index of the last shipped version (HEAD of the project). The
// index is Odin's: a leaf can't write it before the move is committed.
func contractJudgement(ctx context.Context, r *run.Run, gateNode schema.Node) (routing.Judgement, string, error) {
	var p schema.Node
	nodes := r.Nodes()
	for _, m := range nodes[r.State.Position+1:] {
		if m.Kind == schema.KindPointer && m.Task == gateNode.Task {
			p = m
			break
		}
	}
	if p.ID == "" {
		return routing.Judgement{}, "", &nodeFailure{fmt.Errorf("gate %s judges a pointer move, and no pointer node follows it", gateNode.ID)}
	}
	tn, err := tree.Resolve(ctx, r.Setup.Root, filepath.Join(r.Setup.ProjectPath, p.Repo))
	if err != nil {
		return routing.Judgement{}, "", err
	}
	parent, leafNode := tn[len(tn)-2], tn[len(tn)-1]
	child, err := filepath.Rel(parent.Dir, leafNode.Dir)
	if err != nil {
		return routing.Judgement{}, "", err
	}
	l, err := r.Leaf(p.Repo)
	if err != nil {
		return routing.Judgement{}, "", err
	}
	to, err := l.Rev(ctx, "main")
	if err != nil {
		return routing.Judgement{}, "", err
	}
	from, err := git.Run(ctx, parent.Dir, "rev-parse", "HEAD:"+child)
	if err != nil {
		return routing.Judgement{}, "", err
	}
	move := fmt.Sprintf("%s: %.12s -> %.12s", child, from, to)
	if from == to {
		return routing.Judgement{Verdict: routing.Pass, Evidence: routing.Extracted, Reason: "the pointer is already there"}, move, nil
	}
	imp := &routing.Impact{}
	if size, err := git.Run(ctx, parent.Dir, "cat-file", "-s", "HEAD:"+routing.ImpactFile); err == nil {
		// The index is data. One that is too big, or that does not parse, can't be judged by: a person decides.
		n, perr := strconv.Atoi(strings.TrimSpace(size))
		if perr != nil || n > maxIndexBytes {
			return routing.Judgement{Verdict: routing.Escalate, Evidence: routing.Inferred,
				Reason: fmt.Sprintf("the shipped %s is over %d bytes and can't be judged by", routing.ImpactFile, maxIndexBytes)}, move, nil
		}
		b, err := git.Output(ctx, parent.Dir, "show", "HEAD:"+routing.ImpactFile)
		if err != nil {
			return routing.Judgement{}, "", err
		}
		if imp, err = routing.ParseImpact(b); err != nil {
			return routing.Judgement{Verdict: routing.Escalate, Evidence: routing.Inferred,
				Reason: fmt.Sprintf("the shipped %s can't be read (%v), so the move can't be judged", routing.ImpactFile, err)}, move, nil
		}
	}
	names, err := l.Output(ctx, "diff", "-z", "--name-only", "--no-renames", from, to)
	if err != nil {
		return routing.Judgement{}, "", err
	}
	var changes []routing.Change
	for _, f := range strings.Split(string(names), "\x00") {
		if f == "" || !routing.IsContractName(f) {
			continue
		}
		if len(changes) == maxContractChanges {
			return routing.Judgement{Verdict: routing.Escalate, Evidence: routing.Inferred,
				Reason: fmt.Sprintf("the move changes more than %d contract files", maxContractChanges)}, move, nil
		}
		c := routing.Change{Path: path.Join(child, f)}
		if old, err := l.Output(ctx, "show", from+":"+f); err == nil {
			c.Old = old
		}
		if nw, err := l.Output(ctx, "show", to+":"+f); err == nil {
			c.New = nw
		}
		changes = append(changes, c)
	}
	return routing.Judge(ctx, imp, changes, contractDiffer), move, nil
}

// contractFacts runs the check for gate node n, writes pointer_contract_checked, and turns the verdict
// into the facts the gate table reads. The section is for the gate file.
func contractFacts(ctx context.Context, r *run.Run, n schema.Node, attempt int) (gate.Facts, string, error) {
	j, move, err := contractJudgement(ctx, r, n)
	if err != nil {
		return gate.Facts{}, "", err
	}
	ev := gungnir.Event{Name: gungnir.EventPointerContractChecked, Node: n.ID, Attempt: attempt, Task: n.Task,
		Move: move, Evidence: string(j.Evidence), Verdict: string(j.Verdict), Result: j.Reason}
	if err := r.Log(ev); err != nil {
		return gate.Facts{}, "", err
	}
	section := fmt.Sprintf("## Contract evidence\n\n- Move: %s\n- Verdict: %s\n- Evidence: %s\n- Why: %s\n", move, j.Verdict, j.Evidence, j.Reason)
	for _, c := range j.Contracts {
		section += "- Contract: " + c + "\n"
	}
	return gate.Facts{NoContractChange: j.Verdict == routing.Pass, Escalate: j.Verdict == routing.Escalate}, section, nil
}
