package metrics

import (
	"strings"

	"github.com/ouroboric-dev/gungnir"
)

// Ratio is a count out of a total. It has no value when the total is 0: a rate over nothing is not 0%.
type Ratio struct {
	N int `json:"n"`
	D int `json:"d"`
}

// Value is N/D, and false when D is 0.
func (r Ratio) Value() (float64, bool) {
	if r.D == 0 {
		return 0, false
	}
	return float64(r.N) / float64(r.D), true
}

// Loops counts the loops that ended, in a pass or in a give-up.
type Loops struct {
	Finished    int     `json:"finished"`
	Passed      int     `json:"passed"`
	GaveUp      int     `json:"gave_up"`
	FirstPass   Ratio   `json:"first_pass"` // loops that passed in attempt 1, out of the loops that ended
	AvgAttempts float64 `json:"avg_attempts"`
}

// Review is the reviewers' side of the loops.
type Review struct {
	Vetoes Ratio `json:"vetoes"` // attempts in which a reviewer raised a blocker or a major, out of the attempts reviewers saw
}

// Findings counts what the finding events record.
type Findings struct {
	RaisedByCheck      int `json:"raised_by_check"`
	RaisedByReview     int `json:"raised_by_review"`
	RaisedUnattributed int `json:"raised_unattributed"` // lines from before the ledger named an origin
	Closed             int `json:"closed"`
	Reopened           int `json:"reopened"`
	Overruled          int `json:"overruled"`
	DisputesIgnored    int `json:"disputes_ignored"`
}

// Item is one intervention: what kind, where, and what the person or the loop said.
type Item struct {
	Kind string `json:"kind"` // rejection, approval with a note or escalation
	Node string `json:"node"`
	Note string `json:"note"`
}

// Interventions are the times a person had to step in (spec §7.2).
type Interventions struct {
	Rejections     int    `json:"rejections"`
	NotedApprovals int    `json:"noted_approvals"`
	Escalations    int    `json:"escalations"`
	Items          []Item `json:"items"`
}

// Total is every intervention.
func (i Interventions) Total() int { return i.Rejections + i.NotedApprovals + i.Escalations }

// measure fills the loop, review, finding, intervention, grade and routing figures.
func (r *Run) measure(state *record, events []gungnir.Event) error {
	attempts := map[string]int{} // loop node: the attempt it ended in
	end := func(node string, attempt int, passed bool) {
		if _, seen := attempts[node]; !seen {
			r.Loops.Finished++
			r.Loops.AvgAttempts += float64(attempt)
			if passed {
				r.Loops.Passed++
				if attempt == 1 {
					r.Loops.FirstPass.N++
				}
			} else {
				r.Loops.GaveUp++
			}
		}
		attempts[node] = attempt
	}
	loops := map[string]bool{}
	for _, e := range events {
		if e.Name == gungnir.EventLoopAttempt {
			loops[e.Node] = true
		}
	}
	type key struct {
		node    string
		attempt int
	}
	reviewed, vetoed := map[key]bool{}, map[key]bool{}
	for _, e := range events {
		switch e.Name {
		case gungnir.EventLoopAttempt:
			if e.Result == "passed" {
				end(e.Node, e.Attempt, true)
			}
		case gungnir.EventEscalation:
			r.Interventions.Escalations++
			r.Interventions.Items = append(r.Interventions.Items, Item{"escalation", e.Node, e.Result})
			end(e.Node, e.Attempt, false)
		case gungnir.EventGateRejected:
			r.Interventions.Rejections++
			r.Interventions.Items = append(r.Interventions.Items, Item{"rejection", e.Node, e.Note})
		case gungnir.EventGateApproved:
			if e.Note != "" {
				r.Interventions.NotedApprovals++
				r.Interventions.Items = append(r.Interventions.Items, Item{"approval with a note", e.Node, e.Note})
			}
		case gungnir.EventNodeFinished, gungnir.EventNodeFailed:
			if loop, ok := loopOf(e.Node, loops); ok {
				reviewed[key{loop, e.Attempt}] = true
			}
		case gungnir.EventFindingRaised:
			switch e.FindingOrigin {
			case gungnir.OriginCheck:
				r.Findings.RaisedByCheck++
			case gungnir.OriginReview:
				r.Findings.RaisedByReview++
			default:
				r.Findings.RaisedUnattributed++
			}
			fallthrough
		case gungnir.EventFindingReopened:
			if e.FindingOrigin == gungnir.OriginReview && (e.FindingSeverity == string(gungnir.SeverityBlocker) || e.FindingSeverity == string(gungnir.SeverityMajor)) {
				vetoed[key{e.Node, e.Attempt}] = true
			}
			if e.Name == gungnir.EventFindingReopened {
				r.Findings.Reopened++
			}
		case gungnir.EventFindingClosed:
			r.Findings.Closed++
		case gungnir.EventFindingOverruled:
			r.Findings.Overruled++
		case gungnir.EventDisputeIgnored:
			r.Findings.DisputesIgnored++
		}
	}
	if r.Loops.Finished > 0 {
		r.Loops.AvgAttempts /= float64(r.Loops.Finished)
	}
	r.Loops.FirstPass.D = r.Loops.Finished
	for k := range reviewed {
		r.Review.Vetoes.D++
		if vetoed[k] {
			r.Review.Vetoes.N++
		}
	}
	return nil
}

// loopOf finds the loop a reviewer's session belongs to: the session's node is the loop's id, a dash and the
// reviewer's id. The implementer's own sessions are the loop's node itself and are not reviews.
func loopOf(node string, loops map[string]bool) (string, bool) {
	for loop := range loops {
		if strings.HasPrefix(node, loop+"-") {
			return loop, true
		}
	}
	return "", false
}
