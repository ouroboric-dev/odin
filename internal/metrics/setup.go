package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/realm"
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
	Prompts     map[string]string    `json:"prompts"` // a prompt's path in the graph pack: the sha256 of the file
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

// setup fills the record's setup. The graph's and the realms' hashes are the ones the run recorded when it
// loaded them, and a resume refuses a pack whose files changed, so they are the run's. A prompt's hash is read
// from its file, and only while the pack's files still hash as the run recorded: otherwise it would be another
// prompt's, and the record leaves the prompts out and says why. A pinned skill's hash comes from this build's
// library, checked against the set hash the run recorded where it can be.
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
	r.prompts(state, s.Prompts)
	r.skills(state, s.Skills)
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

// prompts hashes every prompt file the graph names from the graph pack: each node's that the pack supplies, and
// each reviewer's.
func (r *Run) prompts(state *record, out map[string]string) {
	if state.GraphDir == "" {
		return
	}
	leave := func(why string) {
		r.Warnings = append(r.Warnings, "the record has no prompt hashes: "+why)
	}
	pack, err := realm.Load(state.GraphDir)
	if err != nil {
		leave(err.Error())
		return
	}
	if pack.Hash != state.GraphPackSHA256 {
		leave(fmt.Sprintf("the graph pack at %s changed since the run started", state.GraphDir))
		return
	}
	g, err := schema.LoadGraph(filepath.Join(pack.Dir, pack.Pack.Graph))
	if err != nil {
		leave(err.Error())
		return
	}
	var paths []string
	for _, n := range g.All() {
		if n.Prompt != "" && n.RealmFrom != schema.RealmLeaf {
			paths = append(paths, n.Prompt)
		}
		for _, rv := range n.Reviewers {
			if rv.Prompt != "" {
				paths = append(paths, rv.Prompt)
			}
		}
	}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(pack.Dir, filepath.FromSlash(p)))
		if err != nil {
			leave(err.Error())
			clear(out)
			return
		}
		h := sha256.Sum256(data)
		out[p] = hex.EncodeToString(h[:])
	}
}

// skills hashes each skill the run could load: a private one as the run recorded it, a pinned one from this
// build's library. With no private skill the pinned ones' set hash must be the run's, or they are left out.
func (r *Run) skills(state *record, out map[string]string) {
	var private []string
	for _, p := range state.Skills.Private {
		out[p.Name] = p.SHA256
		private = append(private, p.Name)
	}
	names := slices.DeleteFunc(slices.Clone(state.Skills.Skills), func(n string) bool { return slices.Contains(private, n) })
	if len(names) == 0 {
		return
	}
	lib, err := havamal.Embedded()
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the record has no hash of the pinned skills: %v", err))
		return
	}
	var pinned []havamal.Entry
	for _, name := range names {
		s, err := lib.Get(name)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("the record has no hash of the skill %s: this build of ygg does not ship it", name))
			continue
		}
		pinned = append(pinned, s.Entry)
		out[name] = havamal.SetHash([]havamal.Entry{s.Entry})
	}
	if len(private) == 0 && len(pinned) > 0 && havamal.SetHash(pinned) != state.Skills.Hash {
		for _, e := range pinned {
			delete(out, e.Name)
		}
		r.Warnings = append(r.Warnings, "the record has no hash of the pinned skills: this build of ygg ships other files for them than the run loaded")
	}
}
