package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/gate"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
)

// reachGate decides gate node n (spec §7.2). Both a stop and an auto-pass write a gate file, so
// unattended decisions can be audited. A stop saves the run as stopped and reports true. An unattended
// block of a task's pointer-contract gate returns a *contractBlock, and the loop gives the task up.
func reachGate(ctx context.Context, r *run.Run, n schema.Node) (bool, error) {
	s := r.State
	attempt, err := startAttempt(r, n)
	if err != nil {
		return false, err
	}
	// The spec gate reads the verdict of the design-conformance review (spec §7.1); no gate knows a
	// contract yet, so ContractInSpec is false. Block only arises for pointer-contract.
	var facts gate.Facts
	var verdict *gungnir.Conformance
	if n.DeviationFrom != "" {
		v, err := loadOutput[gungnir.Conformance](r, n.DeviationFrom)
		if err != nil {
			return false, &nodeFailure{err}
		}
		// A blocked verdict checked nothing, so it stops the gate as a deviation does (ruling R14).
		verdict, facts.Deviation = &v, v.Deviation || v.Blocked != nil
		result := "no deviation"
		switch {
		case v.Blocked != nil:
			result = "blocked: " + firstLine(v.Blocked.Reason)
		case v.Deviation:
			result = "deviation: " + firstLine(v.Amendment)
		}
		if err := r.Log(gungnir.Event{Name: gungnir.EventConformanceChecked, Node: n.DeviationFrom, Attempt: attempt, Result: result}); err != nil {
			return false, err
		}
	}
	var evidence string
	if n.Gate == gungnir.GatePointerContract {
		// The gate judges the move that the next pointer node makes (spec §3.4): a pass opens it, a fail
		// is the approval point, and a guess alone is an escalation.
		var err error
		if facts, evidence, err = contractFacts(ctx, r, n, attempt); err != nil {
			return false, err
		}
	}
	action := gate.Decide(s.Mode, n.Gate, r.Setup.Config.Gates, facts)
	file, err := writeGateFile(r, n, attempt, action, verdict, evidence)
	if err != nil {
		return false, err
	}
	ev := gungnir.Event{Node: n.ID, Attempt: attempt, Gate: n.ID, GateFile: file}
	if action == gate.Auto {
		ev.Name = gungnir.EventGateAutoPassed
		return false, r.Log(ev)
	}
	if action == gate.Block && blocksTasks(r, n) {
		// Unattended: nobody is to approve this. The task is blocked and the run goes on (spec §7.1, §7.3);
		// pointer_contract_checked and task_blocked are the ledger's record of it.
		return false, &contractBlock{why: "its pointer move changes a contract that is not in the approved spec" + contractNames(evidence) +
			". The decision is in " + file + "."}
	}
	ev.Name = gungnir.EventGatePending
	if err := r.Log(ev); err != nil {
		return false, err
	}
	s.Status, s.Pending, s.PendingGate = run.StatusStopped, n.ID, n.Gate
	return true, r.Save()
}

// contractBlock is a pointer-contract gate that an unattended run answered by blocking the task: the
// engine loop gives the task up with why and goes on.
type contractBlock struct{ why string }

func (c *contractBlock) Error() string { return c.why }

// contractNames lists the contracts the gate's evidence section names, as " (a, b)", or "" for none.
func contractNames(evidence string) string {
	var names []string
	for _, line := range strings.Split(evidence, "\n") {
		if name, ok := strings.CutPrefix(line, "- Contract: "); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// writeGateFile writes gates/<node>-<attempt>.md and returns its path relative to the run.
func writeGateFile(r *run.Run, n schema.Node, attempt int, action gate.Action, verdict *gungnir.Conformance, evidence string) (string, error) {
	s := r.State
	rel := filepath.Join("gates", fmt.Sprintf("%s-%d.md", n.ID, attempt))
	var b strings.Builder
	decision := "stopped for a decision"
	blocked := action == gate.Block && blocksTasks(r, n)
	switch {
	case action == gate.Auto:
		decision = "passed automatically"
	case blocked:
		decision = "blocked the task, which an unattended run gives up while the rest goes on"
	}
	fmt.Fprintf(&b, "# Approval point %s (%s)\n\n", n.ID, n.Gate)
	fmt.Fprintf(&b, "- Run: %s\n- Mode: %s\n- Decision: %s\n- Time: %s\n\n", s.ID, s.Mode, decision,
		time.Now().UTC().Format(time.RFC3339))
	b.WriteString("## What happened\n\n")
	nodes := r.Nodes()
	start := 0
	for i := s.Position - 1; i >= 0; i-- {
		if nodes[i].Kind == schema.KindGate {
			start = i + 1
			break
		}
	}
	for _, m := range nodes[start:s.Position] {
		fmt.Fprintf(&b, "- %s (%s, attempt %d): %s\n", m.ID, m.Kind, s.Attempts[m.ID], s.Summaries[m.ID])
	}
	for _, m := range nodes[start:s.Position] {
		section, err := gateSection(r, m.ID)
		if err != nil {
			return "", &nodeFailure{err}
		}
		b.WriteString(section)
	}
	if verdict != nil {
		b.WriteString("\n" + conformanceSection(*verdict))
	}
	if evidence != "" {
		b.WriteString("\n" + evidence)
	}
	u := s.Usage
	fmt.Fprintf(&b, "\n## Cost so far\n\n- Tokens: %d in, %d out, %d from cache\n- Turns: %d\n- Cost: $%.4f",
		u.InTok, u.OutTok, u.CacheTok, u.Turns, u.CostUSD)
	if u.CostEstimated {
		b.WriteString(" (estimated)")
	}
	b.WriteString("\n")
	if blocked {
		fmt.Fprintf(&b, "\n## Blocked\n\nThe task %s is blocked, and the tasks that depend on it. Nothing is waiting for an approval. "+
			"What the task did is kept on a ref for the operator (see the task's reason in the report).\n", n.Task)
	} else if action != gate.Auto {
		fmt.Fprintf(&b, "\n## Decide\n\n    ygg approve %s %s [--note \"...\"]\n    ygg reject %s %s --reason \"...\"\n\n"+
			"A rejection re-runs %s with the reason.\n", s.ID, n.ID, s.ID, n.ID, n.Reruns)
	}
	if err := os.MkdirAll(filepath.Join(r.Dir, "gates"), 0o755); err != nil {
		return "", err
	}
	return rel, os.WriteFile(filepath.Join(r.Dir, rel), []byte(b.String()), 0o644)
}

// gateSection is what a gate shows of the output node kept, for the person who decides there to read: a design's
// open questions, and a reuse review's verdict and its items. It is "" for any other output. The fields are made
// safe as in a view (mdsafe), so none can pass for a section of the gate file.
func gateSection(r *run.Run, node string) (string, error) {
	var b strings.Builder
	switch schema.Contract(r.State.Outputs[node].Contract) {
	case schema.ContractDesign:
		d, err := loadOutput[schema.Design](r, node)
		if err != nil {
			return "", err
		}
		b.WriteString("\n## Open questions of the design\n\n")
		if d.Blocked != nil {
			b.WriteString(mdsafe.Text("The design is blocked: "+d.Blocked.Reason) + "\n\n")
		}
		if len(d.OpenQuestions) == 0 {
			b.WriteString("None.\n")
		}
		for _, q := range d.OpenQuestions {
			b.WriteString("- " + mdsafe.Item(mdsafe.After(mdsafe.Line(q.ID)+": ", q.Question)+"\n(Who can answer: "+mdsafe.Line(q.WhoCanAnswer)+")", 2) + "\n")
		}
	case schema.ContractReuseReview:
		rv, err := loadOutput[schema.ReuseReview](r, node)
		if err != nil {
			return "", err
		}
		b.WriteString("\n## Reuse review\n\n")
		if rv.Blocked != nil {
			b.WriteString(mdsafe.Text("The reuse review is blocked: "+rv.Blocked.Reason) + "\n\n")
		}
		if rv.Verdict != "" {
			b.WriteString(mdsafe.Text("The verdict: "+rv.Verdict) + "\n\n")
		}
		if len(rv.Items) == 0 {
			b.WriteString("It lists no findings.\n")
		}
		for _, it := range rv.Items {
			b.WriteString("- " + mdsafe.Item("Task "+it.Task, 2) + "\n  - " + mdsafe.Item("Adds: "+it.Adds, 4) + "\n  - " +
				mdsafe.Item("Already done by: "+it.AlreadyDoneBy, 4) + "\n  - " + mdsafe.Item("Smallest change: "+it.SmallestChange, 4) + "\n")
		}
	}
	return b.String(), nil
}

func pending(r *run.Run, gateID string) error {
	if r.State.Pending == "" {
		return fmt.Errorf("run %s isn't waiting at an approval point", r.State.ID)
	}
	if r.State.Pending != gateID {
		return fmt.Errorf("run %s is waiting at %s, not %s", r.State.ID, r.State.Pending, gateID)
	}
	return nil
}

// Approve records the decision on the pending approval point, and Advance continues: past a gate
// node; or, at a hard stop a node raised, with that node again. At policy-violation the leaf goes
// back to its checkpoint first; at shrink-guard the move lands once without the guard.
func Approve(ctx context.Context, r *run.Run, gateID, note string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	if err := checkNote(note); err != nil {
		return err
	}
	s := r.State
	n := r.Nodes()[s.Position]
	if n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation {
		if rec := s.Loops[n.ID]; rec == nil || rec.Escalation == nil || !approvableReason(rec.Escalation.Reason) {
			return fmt.Errorf("loop %s stopped because a reviewer's answer can't be read, and an answer nobody can read is not an approval; "+
				"fix the reviewer or its prompt, and then reject the stop to ask it again", n.ID)
		}
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateApproved, Node: gateID, Gate: gateID, Note: note}); err != nil {
		return err
	}
	switch {
	case n.Kind == schema.KindGate:
		// The note of a gate is the product owner's decision (spec §7.2); a note at any other stop is about
		// that stop and stays on the event.
		keepDecision(s, n.ID, note)
		s.Position++
	case n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation:
		// The operator accepts the work with its findings open. They stay open in the ledger: only their
		// source can close a finding.
		rec := s.Loops[n.ID]
		rec.State.Passed, rec.Escalation = true, nil
		s.Summaries[n.ID] = fmt.Sprintf("accepted by the operator after %d attempt(s), with %d finding(s) open", rec.State.Attempt-1, len(rec.State.Book.Open()))
		delete(s.Reasons, n.ID)
		s.Position++
	case n.Kind == schema.KindPublish && s.PendingGate == gungnir.GateRealSecrets:
		// The founder's rule (2026-10-24): the output a scan stopped is kept as Odin validated it, so its source runs
		// again, with the finding as its rejection (set at the stop), and writes it again without what was found. Its
		// new output is kept, published and scanned again; nothing is published while a finding stands.
		if err := runAgainFrom(r, n.From); err != nil {
			return err
		}
	case blockedStop(n, s.PendingGate):
		// A blocked output (ruling R43; a plan at the tasks node, R44): its stage said what only a person can give,
		// and the person's note is the answer. The stage runs again with it as its rejection.
		s.Reasons[n.From] = note
		if strings.TrimSpace(note) == "" {
			s.Reasons[n.From] = blockedRetry
		}
		if err := runAgainFrom(r, n.From); err != nil {
			return err
		}
	case run.Quarantines(s.PendingGate) && n.Repo == "":
		// A node that works in the project: nothing in a leaf to put back, and the node runs again.
	case run.Quarantines(s.PendingGate):
		rec := s.Leaves[n.Repo]
		l, err := r.Leaf(n.Repo)
		if err != nil {
			return err
		}
		if err := l.Reset(ctx, rec.Branch, rec.Checkpoint); err != nil {
			return err
		}
	case s.PendingGate == gungnir.GateShrinkGuard:
		s.Waived[n.ID] = true
	}
	s.Pending, s.PendingGate, s.Status = "", "", run.StatusRunning
	return r.Save()
}

// Rejectable reports whether the run's pending stop takes a rejection: a gate node does, and so does a
// loop that gave up. A hard stop that a node raised takes approve only, or a new run (spec §7.2).
func Rejectable(r *run.Run) bool {
	s := r.State
	if s.Pending == "" || s.Position >= len(r.Nodes()) {
		return false
	}
	n := r.Nodes()[s.Position]
	return n.Kind == schema.KindGate || (n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation)
}

// approvableReason is true for the reasons a person can rule on: attempts that ran out, and a dispute no
// check settled. A reviewer whose answer can't be read is not one.
func approvableReason(reason string) bool {
	return reason == jormungandr.ReasonExhausted || reason == jormungandr.ReasonDisputed
}

// Approvable reports whether the pending stop takes an approval. A loop that gave up because a
// reviewer's answer can't be read does not: that is not a decision a person can take for the reviewer.
func Approvable(r *run.Run) bool {
	s := r.State
	if s.Pending == "" || s.Position >= len(r.Nodes()) {
		return false
	}
	n := r.Nodes()[s.Position]
	if n.Kind == schema.KindLoop && s.PendingGate == gungnir.GateEscalation {
		rec := s.Loops[n.ID]
		return rec != nil && rec.Escalation != nil && approvableReason(rec.Escalation.Reason)
	}
	return true
}

// Reject sends reason back (spec §7.2). At a gate it goes into the node the gate re-runs: Advance
// re-runs it with the reason in its prompt, then reaches the gate again; every loop from that node up
// to the gate starts over. At a loop that gave up (spec §7.3) the reason goes to the loop's implementer,
// and the loop gets a full set of attempts again, with its findings. A hard stop a node raised takes
// no rejection: approve it, or start a new run to drop the work.
func Reject(ctx context.Context, r *run.Run, gateID, reason string) error {
	if err := pending(r, gateID); err != nil {
		return err
	}
	s := r.State
	nodes := r.Nodes()
	if !Rejectable(r) {
		return fmt.Errorf("%s stopped at the hard approval point %s, which takes approve; to drop the work, start a new run", gateID, s.PendingGate)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("a rejection needs a reason; it goes back to the node as input")
	}
	if err := r.Log(gungnir.Event{Name: gungnir.EventGateRejected, Node: gateID, Gate: gateID, Note: reason}); err != nil {
		return err
	}
	if n := nodes[s.Position]; n.Kind == schema.KindLoop {
		rec := s.Loops[n.ID]
		rec.State.Extend(loopConfig(n))
		rec.Escalation = nil
		s.Reasons[n.ID] = reason
	} else {
		s.Reasons[n.Reruns] = reason
		if err := runAgainFrom(r, n.Reruns); err != nil {
			return err
		}
	}
	s.Pending, s.PendingGate, s.Status = "", "", run.StatusRunning
	return r.Save()
}

// runAgainFrom moves the run back from the node it stands at to target, which runs again with its reason in
// s.Reasons. Every loop from the target up to where the run stood starts over: the work under a loop that passed
// changes when a node before it runs again (review I2). The nodes before target are kept.
func runAgainFrom(r *run.Run, target string) error {
	s, nodes := r.State, r.Nodes()
	at := s.Position
	s.Position = indexOf(nodes, target)
	past := map[string]bool{}
	for _, m := range nodes[s.Position:at] {
		if m.Kind == schema.KindGate {
			past[m.ID] = true
		}
		if m.Kind == schema.KindLoop {
			delete(s.Loops, m.ID)
		}
		delete(s.Returns, m.ID) // a rejection is a fresh start: a refusal may go back again
	}
	dropDecisions(s, past) // the run meets those gates again
	return replan(r, nodes[s.Position:at])
}

// blockedRetry is the rejection of a stage whose blocked output a person sent back with no note.
const blockedRetry = "A person read the blocked reason and sent it back to try again."

// blockedStop reports whether node n stopped at gate g for a blocked output of its source: a publish (ruling R43),
// or the tasks node for a blocked plan (R44). Both stop at external-input: a person gives what the stage lacks.
func blockedStop(n schema.Node, g gungnir.Gate) bool {
	return (n.Kind == schema.KindPublish || n.Kind == schema.KindTasks) && g == gungnir.GateExternalInput
}

// hardStop is a node that stops the run at a hard approval point itself: work it left breaks a
// sandbox rule (policy-violation), or a generated file shrank (shrink-guard). Hard approval points
// stop in every mode.
type hardStop struct {
	gate gungnir.Gate
	err  error
}

func (h *hardStop) Error() string { return h.err.Error() }

// journalCap reports whether err is the per-day cap that can't be kept because the day's spend
// journal can't be read or written: raising a cap or waiting for the next day doesn't lift it.
func journalCap(err error) bool {
	c, ok := jormungandr.IsCap(err)
	return ok && c.Dimension == gungnir.CapJournal
}

// stopHard stops the run at node n for the hard approval point h, as a gate node stops: a gate file,
// gate_pending, and the run saved as stopped.
func stopHard(r *run.Run, n schema.Node, h *hardStop) (Outcome, error) {
	s := r.State
	attempt := s.Attempts[n.ID]
	rel := filepath.Join("gates", fmt.Sprintf("%s-%d.md", n.ID, attempt))
	var b strings.Builder
	fmt.Fprintf(&b, "# Hard stop at node %s (%s)\n\n", n.ID, h.gate)
	why := h.err.Error()
	if blockedStop(n, h.gate) {
		why = mdsafe.Text(why) // an agent's words
	}
	fmt.Fprintf(&b, "- Run: %s\n- Mode: %s\n- Time: %s\n\n## Why\n\n%s\n\n", s.ID, s.Mode,
		time.Now().UTC().Format(time.RFC3339), why)
	fmt.Fprintf(&b, "## Decide\n\n    ygg approve %s %s [--note \"...\"]\n\n", s.ID, n.ID)
	switch {
	case n.Kind == schema.KindPublish && h.gate == gungnir.GateRealSecrets:
		fmt.Fprintf(&b, "Nothing was published, and nothing is while a finding stands. What the scan found is in the output of %s. "+
			"Approving runs %s again, the node whose output this is, with the findings above (the rule and the place, never the text) as its rejection: "+
			"its agent writes the output again without the flagged text, and Odin keeps it, publishes it and scans it again. "+
			"The stages before %s are kept. To drop the work, start a new run.\n", n.From, n.From, n.From)
	case blockedStop(n, h.gate):
		if n.Kind == schema.KindTasks {
			b.WriteString("No task was made. ")
		} else {
			b.WriteString("Nothing was published. ")
		}
		fmt.Fprintf(&b, "%s says above what it needs that only a person can give. Give it in the note: approving runs %s again, "+
			"with the note as its rejection (with no note, it is told that a person read the blocked reason and sent it back to try again), "+
			"and the stages before %s are kept.\n", n.From, n.From, n.From)
	case run.Quarantines(h.gate) && n.Repo == "":
		fmt.Fprintf(&b, "Approving runs %s again.\n", n.ID)
	case run.Quarantines(h.gate):
		fmt.Fprintf(&b, "Until then the leaf %s stays as the node left it, for investigation. Approving puts it back to its last checkpoint and runs %s again; what the node wrote is not saved.\n", n.Repo, n.ID)
	case h.gate == gungnir.GateBudgetCap && journalCap(h.err) && len(s.PendingSpend) > 0:
		// A session's spend the journal couldn't take (review I4): the session ran, and its leaf was put back.
		c, _ := jormungandr.IsCap(h.err)
		fmt.Fprintf(&b, "The spend journal can't be written (%s), so the per-day cap can't be kept. "+
			"ygg doctor checks only today's journal file, and a pending spend goes to the file of the day it was spent: the reason above names the path that failed. "+
			"The session's usage is kept in run.json, and is written to the journal first when the run goes on. ", c.Note)
		if len(r.SetAside) == 0 {
			b.WriteString("The session left no uncommitted changes to set aside. ")
		} else {
			fmt.Fprintf(&b, "The session's uncommitted changes were set aside:\n\n- %s\n\n", strings.Join(r.SetAside, "\n- "))
		}
		fmt.Fprintf(&b, "Once the journal can be written, approve: the run runs %s again.\n", n.ID)
	case h.gate == gungnir.GateBudgetCap && journalCap(h.err):
		// The day's journal can't be read: no session started.
		fmt.Fprintf(&b, "Nothing is lost: the work so far is committed and the run can go on. The day's spend journal can't be read, so the per-day cap can't be kept. "+
			"Fix what the reason above names (ygg doctor checks the journal), and then approve: the run goes on with %s.\n", n.ID)
	case h.gate == gungnir.GateBudgetCap:
		fmt.Fprintf(&b, "Nothing is lost: the work so far is committed and the run can go on. Approving does not lift a cap that is still reached. "+
			"Raise the cap in factory.yaml (budget: per_run or per_day), or wait until the day's spend starts over, and then approve: the run goes on with %s.\n", n.ID)
	default:
		b.WriteString("Approving lands this move with the generated files as they are.\n")
	}
	b.WriteString("A hard stop takes no rejection: to drop the work, start a new run.\n")
	if err := os.MkdirAll(filepath.Join(r.Dir, "gates"), 0o755); err != nil {
		return Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, rel), []byte(b.String()), 0o644); err != nil {
		return Outcome{}, err
	}
	ev := gungnir.Event{Name: gungnir.EventGatePending, Node: n.ID, Attempt: attempt, Gate: n.ID, GateFile: rel,
		Note: fmt.Sprintf("%s: %v", h.gate, h.err)}
	if err := r.Log(ev); err != nil {
		return Outcome{}, err
	}
	s.Status, s.Pending, s.PendingGate = run.StatusStopped, n.ID, h.gate
	return Outcome{Status: run.StatusStopped, Gate: n.ID}, r.Save()
}

// indexOf returns the position of the node with the given id, or -1.
func indexOf(nodes []schema.Node, id string) int {
	for i, n := range nodes {
		if n.ID == id {
			return i
		}
	}
	return -1
}
