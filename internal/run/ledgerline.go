package run

import (
	"path/filepath"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

// LogOutside appends e to the ledger of the run in dir and records the new head in run.json, from a command that
// is not advancing the run (ygg score). The caller holds the run's lock (Lock), so no process is writing the
// ledger; the chain is checked against run.json first, as Open does, and nothing else in run.json changes.
func LogOutside(dir string, e gungnir.Event) error {
	s, err := LoadState(dir)
	if err != nil {
		return err
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), s.Ledger)
	if err != nil {
		return err
	}
	e.Run = s.ID
	if err := l.Append(e); err != nil {
		l.Close()
		return err
	}
	s.Ledger = l.Head()
	if err := l.Close(); err != nil {
		return err
	}
	return s.Save(dir)
}
