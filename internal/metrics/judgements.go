package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gungnir/schema"
)

// JudgementsFile is the operator's judgements in the run folder: a list with at most one judgement of the run and
// at most one of each task. The ledger keeps every one that was given; this file holds the last.
const JudgementsFile = "judgements.json"

// ReadJudgements reads the run's judgements in dir; a run nobody judged has none. Each one is decoded against
// ygg.judgement/v1, and a file that holds one that does not decode is an error.
func ReadJudgements(dir string) ([]schema.Judgement, error) {
	path := filepath.Join(dir, JudgementsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]schema.Judgement, 0, len(raw))
	for i, r := range raw {
		j, err := schema.Decode[schema.Judgement](schema.ContractJudgement, r)
		if err != nil {
			return nil, fmt.Errorf("%s: judgement %d: %w", path, i+1, err)
		}
		out = append(out, j)
	}
	return out, nil
}

// WriteJudgements writes the judgements to dir/judgements.json atomically, as a synced temporary file renamed over
// it, with mode 0644 as the run's other files.
func WriteJudgements(dir string, js []schema.Judgement) error {
	if js == nil {
		js = []schema.Judgement{}
	}
	b, err := json.MarshalIndent(js, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, JudgementsFile, append(b, '\n'))
}

// judgementsLeftOut starts the warning of a judgements.json that can't be read; the retrospective's view leaves it out.
const judgementsLeftOut = "the judgements are left out: "

// judgements fills the record with the operator's judgements. A file that can't be read is a warning: the numbers
// stand without it.
func (r *Run) judgements(dir string) {
	js, err := ReadJudgements(dir)
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s%v", judgementsLeftOut, err))
		return
	}
	r.Judgements = js
}
