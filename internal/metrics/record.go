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
	} `json:"realms"`
	Features []schema.FeatureRecord `json:"features"`
	Usage    struct {
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
