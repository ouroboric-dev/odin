package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

// record is the part of run.json that metrics reads. It is read without refusing unknown fields, unlike the
// engine's own State: a comparison reads runs that different builds of ygg wrote, and a build that renamed a
// field must not make an older run unreadable.
type record struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	Mode         string `json:"mode"`
	Graph        string `json:"graph"`
	GraphVersion string `json:"graph_version"`
	ConfigSHA256 string `json:"config_sha256"`
	BriefSHA256  string `json:"brief_sha256"`
	Roles        map[string]struct {
		Agent string `json:"agent"`
		Model string `json:"model"`
	} `json:"roles"`
	Realms map[string]struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	} `json:"realms"`
	Features []schema.FeatureRecord `json:"features"`

	// What the experiment record's setup reads.
	Factory         string            `json:"factory"`
	GraphDir        string            `json:"graph_dir"`
	GraphPackSHA256 string            `json:"graph_pack_sha256"`
	GraphSHA256     string            `json:"graph_sha256"`
	ConfigPath      string            `json:"config_path"`
	PromptSHA256    map[string]string `json:"prompt_sha256"`
	SkillSHA256     map[string]string `json:"skill_sha256"`
	Skills          struct {
		Skills []string `json:"skills"`
	} `json:"skills"`

	// What the tasks' sizes and the harness read.
	Root        string `json:"root"`
	ProjectPath string `json:"project"`
	Loops       map[string]*struct {
		Base string `json:"base"`
	} `json:"loops"`
	Decisions []struct {
		Gate      string           `json:"gate"`
		Decisions schema.Decisions `json:"decisions"`
	} `json:"decisions"`
	Outputs map[string]struct {
		Contract string `json:"contract"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
	} `json:"outputs"`
	Usage struct {
		InTok  int64 `json:"in_tok"`
		OutTok int64 `json:"out_tok"`
		Turns  int64 `json:"turns"`
	} `json:"usage"`
	Ledger ledger.Anchor `json:"ledger"`
}

func loadRecord(dir string) (*record, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "run.json"), err)
	}
	if !strings.HasPrefix(rec.Schema, "ygg.run/") {
		return nil, fmt.Errorf("%s: schema %q is not a ygg run record", filepath.Join(dir, "run.json"), rec.Schema)
	}
	return &rec, nil
}

// RecordFile is the experiment record's file in the run folder.
const RecordFile = "record.json"

// Encode is r as the run's experiment record (ygg.run-record/v1): what ygg record writes and ygg metrics --json
// prints. A record that does not validate is Odin's own error, and is never written.
func Encode(r *Run) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if err := schema.Validate(schema.ContractRecord, b); err != nil {
		return nil, fmt.Errorf("the record of run %s does not validate: %w", r.ID, err)
	}
	return b, nil
}

// WriteRecord rebuilds the experiment record of the run in dir from its ledger, run.json and kept files, and
// writes it to dir/record.json atomically: a synced temporary file in dir, renamed over it. It returns the path.
func WriteRecord(dir string) (string, error) {
	r, err := Collect(dir)
	if err != nil {
		return "", err
	}
	return Write(dir, r)
}

// Write writes r, the record Collect built of the run in dir, to dir/record.json as WriteRecord does, and returns the
// path. A record that does not validate is not written.
func Write(dir string, r *Run) (string, error) {
	b, err := Encode(r)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(dir, RecordFile, b); err != nil {
		return "", err
	}
	return filepath.Join(dir, RecordFile), nil
}

// Objective is the record as the retrospective reads it (change typed-boundaries §5): its objective part, without
// the operator's judgements, the warning about them, and the retrospective's own answer, and of the hidden suite the
// score's totals only. The grade keeps no category of the suite, and every field it keeps is named here, so a field
// added to Grade later stays out until it is added on purpose. r is not changed.
func (r *Run) Objective() *Run {
	o := *r
	o.Judgements, o.Retro = nil, nil
	o.Warnings = []string{}
	for _, w := range r.Warnings {
		if !strings.HasPrefix(w, judgementsLeftOut) {
			o.Warnings = append(o.Warnings, w)
		}
	}
	if g := r.Grade; g != nil {
		o.Grade = &Grade{Task: g.Task, TaskVersion: g.TaskVersion, Suite: g.Suite, Reward: g.Reward, FailToPass: g.FailToPass,
			PassToPass: g.PassToPass, Categories: map[string]Category{}, Duration: g.Duration, EscapedDefects: g.EscapedDefects,
			TasksGivenUp: g.TasksGivenUp, Refused: g.Refused}
	}
	return &o
}

// writeAtomic writes data to dir/name with mode 0644: a synced temporary file in dir, renamed over the name, and
// the directory synced.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil { // as the run's other files; CreateTemp makes it 0600
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
