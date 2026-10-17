package budget

import (
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// Meter reads a run's usage and the day's journal against the caps in factory.yaml. It is a
// gungnir.Meter: Jormungandr asks it before every session.
type Meter struct {
	Run     *gungnir.Usage // the run's usage so far, which Odin keeps in run.json
	Caps    schema.Budget
	USD     bool // dollars count: a role bills a paid API (see schema.PaidAPI)
	Journal *Journal
}

var _ gungnir.Meter = (*Meter)(nil)

// Check returns the first cap that is reached, the run's before the day's, or nil. The day's
// figure comes from the journal; when the journal can't be read the per-day cap can't be checked,
// and that stops the run too (gungnir.CapJournal).
func (m *Meter) Check() *gungnir.CapError {
	if hit := m.Caps.PerRun.Reached(gungnir.CapRun, *m.Run, m.USD); hit != nil {
		return hit
	}
	day, err := m.Journal.Today()
	if err != nil {
		return &gungnir.CapError{Scope: gungnir.CapDay, Dimension: gungnir.CapJournal, Note: err.Error()}
	}
	return m.Caps.PerDay.Reached(gungnir.CapDay, day, m.USD)
}

// Today returns what the journal holds for today.
func (m *Meter) Today() (gungnir.Usage, error) { return m.Journal.Today() }

// RemainingTurns returns how many turns the caps still allow: the lesser of the run's and the
// day's, always above zero. It bounds the turns of a session, so one session can't run far past a
// cap that Check only sees afterwards. When no turn is left it returns the cap that says why, the
// run's before the day's, as Check does; an unreadable journal leaves none (gungnir.CapJournal).
// The turns can run out after Check let a session start: another run can spend the day's last ones.
func (m *Meter) RemainingTurns() (int64, *gungnir.CapError) {
	if m.Run.Turns >= m.Caps.PerRun.Turns {
		return 0, &gungnir.CapError{Scope: gungnir.CapRun, Dimension: gungnir.CapTurns, Used: float64(m.Run.Turns), Limit: float64(m.Caps.PerRun.Turns)}
	}
	day, err := m.Today()
	if err != nil {
		return 0, &gungnir.CapError{Scope: gungnir.CapDay, Dimension: gungnir.CapJournal, Note: err.Error()}
	}
	if day.Turns >= m.Caps.PerDay.Turns {
		return 0, &gungnir.CapError{Scope: gungnir.CapDay, Dimension: gungnir.CapTurns, Used: float64(day.Turns), Limit: float64(m.Caps.PerDay.Turns)}
	}
	return min(m.Caps.PerRun.Turns-m.Run.Turns, m.Caps.PerDay.Turns-day.Turns), nil
}

// Near returns the caps that are at least frac of the way reached, and not yet reached, for a
// warning (spec §10.1): the usage in each CapError is the figure, the limit the cap.
func (m *Meter) Near(frac float64) []gungnir.CapError {
	var near []gungnir.CapError
	day, _ := m.Today()
	for _, s := range []struct {
		scope gungnir.CapScope
		cap   schema.Cap
		use   gungnir.Usage
	}{{gungnir.CapRun, m.Caps.PerRun, *m.Run}, {gungnir.CapDay, m.Caps.PerDay, day}} {
		dims := []gungnir.CapError{
			{Scope: s.scope, Dimension: gungnir.CapTokens, Used: float64(s.use.CapTokens()), Limit: float64(s.cap.Tokens)},
			{Scope: s.scope, Dimension: gungnir.CapTurns, Used: float64(s.use.Turns), Limit: float64(s.cap.Turns)},
		}
		if m.USD {
			dims = append(dims, gungnir.CapError{Scope: s.scope, Dimension: gungnir.CapUSD, Used: s.use.CostUSD, Limit: s.cap.USD})
		}
		for _, d := range dims {
			if d.Limit > 0 && d.Used >= frac*d.Limit && d.Used < d.Limit {
				near = append(near, d)
			}
		}
	}
	return near
}
