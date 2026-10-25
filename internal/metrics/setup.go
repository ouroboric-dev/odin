package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// RoleSetup is one role's agent, model, CLI version and reasoning.
type RoleSetup struct {
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	CLIVersion string `json:"cli_version,omitempty"` // what the role's sessions reported; none for an adapter that reports none
	Reasoning  string `json:"reasoning,omitempty"`   // as factory.yaml set it; none is the adapter's default
}

// Setup is what a run was set up with: each role, and a fingerprint of each thing its agents were given. Two runs
// with the same setup ran the same factory on the same task, so their numbers compare.
type Setup struct {
	Roles       map[string]RoleSetup `json:"roles"`
	Graph       map[string]string    `json:"graph"`   // graph name: the sha256 of its pack's files, as the run recorded it
	Prompts     map[string]string    `json:"prompts"` // a prompt's path in the graph pack, or <realm>:<path>: the sha256 the run read
	Skills      map[string]string    `json:"skills"`  // skill name: the sha256 of its files (havamal.SetHash of the one skill)
	Realms      map[string]string    `json:"realms"`  // realm name: the sha256 of its files, as the run recorded it
	Task        string               `json:"task,omitempty"`
	TaskVersion string               `json:"task_version,omitempty"`
	Factory     string               `json:"factory,omitempty"` // the build of ygg that started the run
}

func newSetup() Setup {
	return Setup{Roles: map[string]RoleSetup{}, Graph: map[string]string{}, Prompts: map[string]string{}, Skills: map[string]string{},
		Realms: map[string]string{}}
}

// setup fills the record's setup, from what run.json kept when the run started: the graph's and the realms' hashes,
// which a resume checks, and the hash of each prompt and skill the run read. Only the roles' reasoning is read from
// factory.yaml, and only while the file is the one the run last used.
func (r *Run) setup(state *record, events []gungnir.Event) {
	s := newSetup()
	cli := map[string]string{}
	for _, e := range events {
		if e.Role != "" && e.CLIVersion != "" {
			cli[e.Role] = e.CLIVersion
		}
	}
	reasoning := r.reasoning(state)
	for name, rr := range state.Roles {
		s.Roles[name] = RoleSetup{Agent: rr.Agent, Model: rr.Model, CLIVersion: cli[name], Reasoning: reasoning[name]}
	}
	if state.Graph != "" && state.GraphPackSHA256 != "" {
		s.Graph[state.Graph] = state.GraphPackSHA256
	}
	for name, rl := range state.Realms {
		if rl.SHA256 != "" {
			s.Realms[name] = rl.SHA256
		}
	}
	r.fingerprints(state, &s)
	if r.Grade != nil {
		s.Task, s.TaskVersion = r.Grade.Task, r.Grade.TaskVersion
	}
	s.Factory = state.Factory
	if s.Factory == "" && len(r.Factory) > 0 {
		s.Factory = r.Factory[0]
	}
	r.Setup = s
}

// reasoning reads each role's reasoning from factory.yaml, if the file is still the one the run last used.
func (r *Run) reasoning(state *record) map[string]string {
	out := map[string]string{}
	if state.ConfigPath == "" {
		return out
	}
	data, err := os.ReadFile(state.ConfigPath)
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the roles' reasoning is left out: %v", err))
		return out
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != state.ConfigSHA256 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the roles' reasoning is left out: %s changed since the run last used it", state.ConfigPath))
		return out
	}
	f, err := schema.ParseFactory(state.ConfigPath, data)
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the roles' reasoning is left out: %v", err))
		return out
	}
	for name, role := range f.Roles {
		if role.Reasoning != "" {
			out[name] = role.Reasoning
		}
	}
	return out
}

// fingerprints copies the hashes of the prompts and skills that run.json kept when the run started, from the bytes
// the run read (ruling R55). The record never reads the files again: they may have changed since. A run from before
// run.json kept them has none, and the record says so.
func (r *Run) fingerprints(state *record, s *Setup) {
	maps.Copy(s.Prompts, state.PromptSHA256)
	maps.Copy(s.Skills, state.SkillSHA256)
	if state.GraphDir != "" && len(state.PromptSHA256) == 0 {
		r.Warnings = append(r.Warnings, "the record has no prompt or skill hashes: run.json keeps no hash of its prompts, so the run started before ygg kept them")
	}
}
