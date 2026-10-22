package metrics

import (
	"slices"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

// clock sets the run's times. The clock ends at the last run_finished, so grading a day later is not run time.
func (r *Run) clock(events []gungnir.Event) {
	start, end := events[0].Time, events[len(events)-1].Time
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Name == gungnir.EventRunFinished {
			end = events[i].Time
			events = events[:i+1]
			break
		}
	}
	approval, stopped := idle(events, end)
	elapsed, wait := end.Sub(start), length(approval, start, end)
	all := length(slices.Concat(approval, stopped), start, end)
	r.Time = Time{Elapsed: Duration(elapsed), ApprovalWait: Duration(wait), Stopped: Duration(all - wait), Active: Duration(elapsed - all)}
}

type span struct{ from, to time.Time }

// idle finds the time in which no one was working on the run: the approval waits (a gate_pending until the
// decision that follows it) and the stops (a failure or a pause until the resume, or the gap before a resume
// that no failure line explains, because the process was killed). end is the run's last counted line; a wait
// still open then counts for nothing, because the clock has not reached its end.
func idle(events []gungnir.Event, end time.Time) (approval, stopped []span) {
	for i, e := range events {
		switch e.Name {
		case gungnir.EventGatePending:
			if j := next(events, i, gungnir.EventGateApproved, gungnir.EventGateRejected); j >= 0 {
				approval = append(approval, span{e.Time, events[j].Time})
			}
		case gungnir.EventRunPaused, gungnir.EventRunFailed:
			if j := next(events, i, gungnir.EventRunResumed); j >= 0 {
				stopped = append(stopped, span{e.Time, events[j].Time})
			}
		case gungnir.EventRunResumed:
			if i > 0 && !slices.Contains([]gungnir.EventName{gungnir.EventRunPaused, gungnir.EventRunFailed, gungnir.EventGatePending}, events[i-1].Name) {
				stopped = append(stopped, span{events[i-1].Time, e.Time})
			}
		}
	}
	_ = end
	return approval, stopped
}

// next is the index of the first line after i that has one of the names, or -1.
func next(events []gungnir.Event, i int, names ...gungnir.EventName) int {
	for j := i + 1; j < len(events); j++ {
		if slices.Contains(names, events[j].Name) {
			return j
		}
	}
	return -1
}

// length is the total time the spans cover, each moment once, between from and to.
func length(spans []span, from, to time.Time) time.Duration {
	var clipped []span
	for _, s := range spans {
		s.from, s.to = maxTime(s.from, from), minTime(s.to, to)
		if s.to.After(s.from) {
			clipped = append(clipped, s)
		}
	}
	slices.SortFunc(clipped, func(a, b span) int { return a.from.Compare(b.from) })
	var total time.Duration
	var cur *span
	for i := range clipped {
		s := clipped[i]
		switch {
		case cur == nil:
			cur = &s
		case s.from.After(cur.to):
			total += cur.to.Sub(cur.from)
			cur = &s
		case s.to.After(cur.to):
			cur.to = s.to
		}
	}
	if cur != nil {
		total += cur.to.Sub(cur.from)
	}
	return total
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
