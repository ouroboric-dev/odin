// Package budget keeps the factory's spend and decides when a cap is reached (spec §7.4). Every
// session's usage is appended to a journal, one file per day, outside any project: the per-day cap
// counts what every run of the operator spent that day, in every project, because the operator's
// subscription is one (Phase 4, decision 5).
package budget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

// Entry is one line of the journal: one session's usage.
type Entry struct {
	Time    time.Time     `json:"time"`
	Run     string        `json:"run"`
	Project string        `json:"project"`
	Node    string        `json:"node"`
	Usage   gungnir.Usage `json:"usage"`
}

// Journal is the folder of daily spend files, YYYY-MM-DD.jsonl.
type Journal struct {
	Dir string
	// Now is the time of an entry; time.Now by default. Loc is the operator's day: a day ends at
	// midnight there, time.Local by default.
	Now func() time.Time
	Loc *time.Location
}

// DefaultDir is where the journal lives when factory.yaml sets none: $XDG_STATE_HOME/ygg/spend, or
// ~/.local/state/ygg/spend. It is an error when neither can be found, since a spend that can't be
// recorded can't be capped.
func DefaultDir(getenv func(string) string, home func() (string, error)) (string, error) {
	if x := getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "ygg", "spend"), nil
	}
	h, err := home()
	if err != nil || !filepath.IsAbs(h) {
		return "", errors.New("can't find the operator's state folder: set budget.journal in factory.yaml to an absolute folder")
	}
	return filepath.Join(h, ".local", "state", "ygg", "spend"), nil
}

func (j *Journal) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Journal) loc() *time.Location {
	if j.Loc != nil {
		return j.Loc
	}
	return time.Local
}

// Today adds up the usage recorded today.
func (j *Journal) Today() (gungnir.Usage, error) { return j.Day(j.now()) }

// Date names the day of now, such as 2026-10-16, in the operator's zone.
func (j *Journal) Date() string { return j.now().In(j.loc()).Format("2006-01-02") }

// File returns the journal file of the day t falls in.
func (j *Journal) File(t time.Time) string {
	return filepath.Join(j.Dir, t.In(j.loc()).Format("2006-01-02")+".jsonl")
}

// Stamp returns e with its time set to now when it has none, so an entry appended later, after the
// journal failed, still belongs to the day it was spent.
func (j *Journal) Stamp(e Entry) Entry {
	if e.Time.IsZero() {
		e.Time = j.now().UTC()
	}
	return e
}

// Append records one session's usage, now. The line is written with one write to a file opened for
// appending, and synced, so lines from runs going at once never mix and a crash loses nothing that
// was returned.
func (j *Journal) Append(e Entry) error {
	e = j.Stamp(e)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(j.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.File(e.Time), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Day adds up the usage recorded on the day t falls in. A last line cut short, which a crash leaves,
// is ignored; any other line that is not an entry is an error, because a sum that skips lines would
// under-count the spend, and a cap must err the other way.
func (j *Journal) Day(t time.Time) (gungnir.Usage, error) {
	var sum gungnir.Usage
	path := j.File(t)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sum, nil
	}
	if err != nil {
		return sum, err
	}
	lines := bytes.Split(data, []byte("\n"))
	if last := len(lines) - 1; len(lines[last]) > 0 {
		lines = lines[:last] // no newline after it: the write was cut short
	}
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var e Entry
		if err := dec.Decode(&e); err != nil || !finite(e.Usage) {
			return sum, fmt.Errorf("%s:%d is not a spend entry; fix or remove the line", path, i+1)
		}
		sum = sum.Add(e.Usage)
	}
	return sum, nil
}

// finite reports whether u is a usage a session could have had.
func finite(u gungnir.Usage) bool {
	return u.InTok >= 0 && u.OutTok >= 0 && u.CacheTok >= 0 && u.Turns >= 0 &&
		u.CostUSD >= 0 && !math.IsNaN(u.CostUSD) && !math.IsInf(u.CostUSD, 0)
}
