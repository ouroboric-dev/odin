package metrics

import (
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

func passed(node string, attempt int) gungnir.Event {
	return gungnir.Event{Name: gungnir.EventLoopAttempt, Node: node, Attempt: attempt, Result: "passed"}
}

func review(node, reviewer string, attempt int) gungnir.Event {
	e := session(node+"-"+reviewer, "reviewer", "opencode", "openrouter/qwen/qwen3.8-27b", 100, 10, 2, 0.01, false)
	e.Attempt = attempt
	return e
}

func finding(name gungnir.EventName, node string, attempt int, id, source string, sev gungnir.Severity, origin gungnir.FindingOrigin) gungnir.Event {
	return gungnir.Event{Name: name, Node: node, Attempt: attempt, FindingID: id, FindingSource: source, FindingSeverity: string(sev), FindingOrigin: origin}
}

func TestFirstPassRateAndAverageAttemptsCountEveryLoopThatEnded(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, passed("a-implement", 1))                                                                                    // first time
	f.log(2, passed("b-implement", 3))                                                                                    // third attempt
	f.log(3, gungnir.Event{Name: gungnir.EventEscalation, Node: "c-implement", Attempt: 3, Result: "attempts-exhausted"}) // gave up
	f.log(3, passed("e-implement", 2))                                                                                    // second attempt: a pass, not a first pass
	f.log(4, gungnir.Event{Name: gungnir.EventLoopAttempt, Node: "d-implement", Attempt: 1, Result: "attempt 1 of 3 starts with 0 finding(s) open"})
	r := mustCollect(t, f.done())
	l := r.Loops
	if l.Finished != 4 || l.Passed != 3 || l.GaveUp != 1 || l.FirstPass != (Ratio{1, 4}) || !near(l.AvgAttempts, 9.0/4) {
		t.Errorf("Loops = %+v; want 4 ended, 3 passed, 1 gave up, first pass 1/4, 9/4 attempts on average", l)
	}
	if v, ok := l.FirstPass.Value(); !ok || !near(v, 0.25) {
		t.Errorf("FirstPass.Value() = %v, %v", v, ok)
	}
	if _, ok := (Ratio{0, 0}).Value(); ok {
		t.Error("a ratio of nothing over nothing must say it has no value, not 0")
	}
}

// Spec §10.2: the reviewer veto rate. A veto is an attempt in which a reviewer raised a blocking finding;
// a check that fails is not a reviewer's veto.
func TestAReviewerVetoIsAnAttemptWhereAReviewerRaisedABlockingFinding(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, review("x-implement", "quality", 1))
	f.log(2, finding(gungnir.EventFindingRaised, "x-implement", 1, "q-1", "quality", gungnir.SeverityMajor, gungnir.OriginReview))
	f.log(3, review("x-implement", "quality", 2))
	f.log(3, review("x-implement", "spec", 2))
	f.log(4, finding(gungnir.EventFindingRaised, "x-implement", 2, "q-2", "quality", gungnir.SeverityMinor, gungnir.OriginReview)) // minor: no veto
	f.log(5, finding(gungnir.EventFindingRaised, "x-implement", 2, "check-unit", "unit", gungnir.SeverityBlocker, gungnir.OriginCheck))
	f.log(6, passed("x-implement", 2))
	r := mustCollect(t, f.done())
	if r.Review.Vetoes != (Ratio{1, 2}) {
		t.Errorf("Review.Vetoes = %+v; want 1 of 2 reviewed attempts", r.Review.Vetoes)
	}
	fs := r.Findings
	if fs.RaisedByReview != 2 || fs.RaisedByCheck != 1 || fs.RaisedUnattributed != 0 {
		t.Errorf("Findings = %+v; want 2 by review and 1 by a check", fs)
	}
}

func TestFindingsAndDisputesAreCountedFromTheirOwnEvents(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, finding(gungnir.EventFindingRaised, "x-implement", 1, "a", "spec", gungnir.SeverityBlocker, ""))
	f.log(2, finding(gungnir.EventFindingClosed, "x-implement", 2, "a", "spec", gungnir.SeverityBlocker, gungnir.OriginReview))
	f.log(3, finding(gungnir.EventFindingReopened, "x-implement", 3, "a", "spec", gungnir.SeverityBlocker, gungnir.OriginReview))
	f.log(4, finding(gungnir.EventFindingOverruled, "x-implement", 3, "a", "spec", gungnir.SeverityBlocker, gungnir.OriginReview))
	f.log(5, gungnir.Event{Name: gungnir.EventDisputeIgnored, Node: "x-implement", Attempt: 3, Note: "dispute ignored: the check is not run"})
	f.log(6, gungnir.Event{Name: gungnir.EventRetry, Note: "429"})
	r := mustCollect(t, f.done())
	fs := r.Findings
	if fs.RaisedUnattributed != 1 || fs.Closed != 1 || fs.Reopened != 1 || fs.Overruled != 1 || fs.DisputesIgnored != 1 || r.Retries != 1 {
		t.Errorf("Findings = %+v, Retries = %d", fs, r.Retries)
	}
}

// Spec §7.2: rejections, approvals with notes and escalations are interventions. A plain approval is not.
func TestInterventionsAreRejectionsNotedApprovalsAndEscalations(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventGateApproved, Node: "design-gate", Gate: "design-gate"})
	f.log(2, gungnir.Event{Name: gungnir.EventGateApproved, Node: "x-contract-gate", Gate: "x-contract-gate", Note: "the contract change is in the spec"})
	f.log(3, gungnir.Event{Name: gungnir.EventGateRejected, Node: "plan-gate", Gate: "plan-gate", Note: "split task 3"})
	f.log(4, gungnir.Event{Name: gungnir.EventGateAutoPassed, Node: "feature-gate", Gate: "feature-gate"})
	f.log(5, gungnir.Event{Name: gungnir.EventEscalation, Node: "y-implement", Attempt: 3, Result: "attempts-exhausted"})
	got := mustCollect(t, f.done()).Interventions
	if got.Rejections != 1 || got.NotedApprovals != 1 || got.Escalations != 1 || got.Total() != 3 {
		t.Errorf("Interventions = %+v; want 1 rejection, 1 noted approval, 1 escalation, 3 in all", got)
	}
}

func TestEachInterventionIsListedWithWhereItHappenedAndItsNote(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventGateApproved, Node: "x-contract-gate", Gate: "x-contract-gate", Note: "the contract change is in the spec"})
	f.log(2, gungnir.Event{Name: gungnir.EventGateRejected, Node: "plan-gate", Gate: "plan-gate", Note: "split task 3"})
	f.log(3, gungnir.Event{Name: gungnir.EventEscalation, Node: "y-implement", Attempt: 3, Result: "attempts-exhausted"})
	f.log(4, gungnir.Event{Name: gungnir.EventGateApproved, Node: "design-gate", Gate: "design-gate"}) // plain: not an intervention
	got := mustCollect(t, f.done()).Interventions.Items
	want := []Item{{"approval with a note", "x-contract-gate", "the contract change is in the spec"}, {"rejection", "plan-gate", "split task 3"}, {"escalation", "y-implement", "attempts-exhausted"}}
	if len(got) != len(want) {
		t.Fatalf("Items = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Items[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
