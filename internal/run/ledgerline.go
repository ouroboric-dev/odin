package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

// LogOutside appends e to the ledger of the run in dir and records the new head in run.json, from a command that
// is not advancing the run (ygg score). The caller holds the run's lock (Lock), so no process is writing the
// ledger; the chain is checked against run.json first, as Open does, and nothing else in run.json changes: a run
// from before typed boundaries keeps its shape, so resume still refuses it (ruling R49).
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
	head := l.Head()
	if err := l.Close(); err != nil {
		return err
	}
	return saveAnchor(dir, head)
}

// saveAnchor replaces the ledger anchor of dir/run.json with head and keeps every other field as it was written.
func saveAnchor(dir string, head ledger.Anchor) error {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("%s/run.json: %w", dir, err)
	}
	if fields["ledger"], err = json.Marshal(head); err != nil {
		return err
	}
	if data, err = json.MarshalIndent(fields, "", "  "); err != nil {
		return err
	}
	return writeState(dir, data)
}
