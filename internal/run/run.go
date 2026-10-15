package run

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/agents"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

// Options say what to run.
type Options struct {
	Project string       // the directory that holds .factory/ and workspace/
	Config  string       // factory.yaml
	Realm   string       // the directory that holds pack.yaml
	Brief   string       // optional
	Mode    gungnir.Mode // "" means the config's mode
}

// Setup is everything a run needs, loaded and checked.
type Setup struct {
	Config       *schema.Factory
	ConfigPath   string // absolute
	ConfigSHA256 string
	Pack         *schema.Pack
	RealmDir     string // absolute
	Graph        *schema.Graph
	GraphSHA256  string
	Brief        string // absolute, or ""
	BriefText    string
	BriefSHA256  string
	Mode         gungnir.Mode
	Agents       map[string]gungnir.Agent // role: adapter, for every role the graph uses
}

// Prepare loads and checks the config, the realm and the brief, and builds the agents. It writes
// nothing. Incomplete caps give a *schema.CapsError (change require-budget-caps).
func Prepare(o Options) (*Setup, error) {
	s := &Setup{Agents: map[string]gungnir.Agent{}}
	var err error
	if s.ConfigPath, err = filepath.Abs(o.Config); err != nil {
		return nil, err
	}
	cfgData, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	if s.Config, err = schema.ParseFactory(s.ConfigPath, cfgData); err != nil {
		return nil, err
	}
	s.ConfigSHA256 = sum(cfgData)
	s.Mode = s.Config.Mode
	if o.Mode != "" {
		s.Mode = o.Mode
	}
	if err := s.Config.CheckGates(s.Mode); err != nil {
		return nil, fmt.Errorf("%s: %w", s.ConfigPath, err)
	}
	if s.RealmDir, err = filepath.Abs(o.Realm); err != nil {
		return nil, err
	}
	if s.Pack, err = schema.LoadPack(s.RealmDir); err != nil {
		return nil, err
	}
	graphPath := filepath.Join(s.RealmDir, s.Pack.Graph)
	graphData, err := os.ReadFile(graphPath)
	if err != nil {
		return nil, err
	}
	if s.Graph, err = schema.ParseGraph(graphPath, graphData); err != nil {
		return nil, err
	}
	s.GraphSHA256 = sum(graphData)
	for _, n := range s.Graph.Nodes {
		if n.Kind != schema.KindAgent {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.RealmDir, n.Prompt)); err != nil {
			return nil, fmt.Errorf("%s: node %s: %w", graphPath, n.ID, err)
		}
		if s.Agents[n.Role] != nil {
			continue
		}
		role, ok := s.Config.Roles[n.Role]
		if !ok {
			return nil, fmt.Errorf("%s: node %s uses role %s, which %s doesn't define", graphPath, n.ID, n.Role, s.ConfigPath)
		}
		if s.Agents[n.Role], err = agents.New(role, filepath.Dir(s.ConfigPath)); err != nil {
			return nil, fmt.Errorf("role %s: %w", n.Role, err)
		}
	}
	if o.Brief != "" {
		if s.Brief, err = filepath.Abs(o.Brief); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(s.Brief)
		if err != nil {
			return nil, err
		}
		s.BriefText, s.BriefSHA256 = string(b), sum(b)
	}
	return s, nil
}

// Run is an open run: its directory, state, setup, ledger and lock.
type Run struct {
	Project string
	Dir     string
	State   *State
	Setup   *Setup
	Ledger  *ledger.Ledger
	unlock  func()
}

// Workspace is the directory agents and exec nodes work in. Phase 2 replaces it with the leaf.
func (r *Run) Workspace() string { return filepath.Join(r.Project, "workspace") }

// Log appends e to the ledger, with the run's id.
func (r *Run) Log(e gungnir.Event) error {
	e.Run = r.State.ID
	return r.Ledger.Append(e)
}

// Save records the ledger's head in run.json and writes it.
func (r *Run) Save() error {
	r.State.Ledger = r.Ledger.Head()
	r.State.Updated = time.Now().UTC()
	return r.State.Save(r.Dir)
}

// Close closes the ledger and releases the lock.
func (r *Run) Close() error {
	err := r.Ledger.Close()
	r.unlock()
	return err
}

// Start prepares a new run, then creates its directory, run.json and first ledger line. If
// Prepare refuses, nothing is created.
func Start(o Options) (*Run, error) {
	setup, err := Prepare(o)
	if err != nil {
		return nil, err
	}
	project, err := filepath.Abs(o.Project)
	if err != nil {
		return nil, err
	}
	id, err := NewIDNow()
	if err != nil {
		return nil, err
	}
	dir := Dir(project, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(project, "workspace"), 0o755); err != nil {
		return nil, err
	}
	unlock, err := Lock(dir)
	if err != nil {
		return nil, err
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), ledger.Anchor{})
	if err != nil {
		unlock()
		return nil, err
	}
	now := time.Now().UTC()
	roles := map[string]RoleRecord{}
	for name := range setup.Agents {
		roles[name] = RoleRecord{Agent: setup.Config.Roles[name].Agent, Model: setup.Config.Roles[name].Model}
	}
	r := &Run{Project: project, Dir: dir, Setup: setup, Ledger: l, unlock: unlock, State: &State{
		ID: id, Status: StatusRunning, Mode: setup.Mode,
		Realm: setup.Pack.Name, RealmVersion: setup.Pack.Version, RealmDir: setup.RealmDir,
		GraphVersion: setup.Graph.Version, GraphSHA256: setup.GraphSHA256,
		ConfigPath: setup.ConfigPath, ConfigSHA256: setup.ConfigSHA256,
		Brief: setup.Brief, BriefSHA256: setup.BriefSHA256, Roles: roles,
		Attempts: map[string]int{}, Reasons: map[string]string{}, Summaries: map[string]string{},
		Created: now, Updated: now,
	}}
	if err := r.Log(gungnir.Event{Name: gungnir.EventRunStarted, Mode: setup.Mode, ConfigHash: setup.ConfigSHA256}); err != nil {
		r.Close()
		return nil, err
	}
	return r, r.Save()
}

// Open locks and loads run id for resume, approve or reject, and checks that it may continue: the
// caps are still complete, the graph and brief are unchanged, and the ledger reaches the anchor in
// run.json. mode switches the run's mode at this pause ("" keeps it); a switch is logged.
func Open(project, id string, mode gungnir.Mode) (*Run, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%q is not a run id", id)
	}
	project, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	dir := Dir(project, id)
	if _, err := os.Stat(filepath.Join(dir, "run.json")); err != nil {
		return nil, fmt.Errorf("no run %s in %s", id, project)
	}
	unlock, err := Lock(dir)
	if err != nil {
		return nil, err
	}
	r, err := open(project, dir, mode)
	if err != nil {
		unlock()
		return nil, err
	}
	r.unlock = unlock
	return r, nil
}

func open(project, dir string, mode gungnir.Mode) (*Run, error) {
	s, err := LoadState(dir)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = s.Mode
	}
	setup, err := Prepare(Options{Project: project, Config: s.ConfigPath, Realm: s.RealmDir, Brief: s.Brief, Mode: mode})
	if err != nil {
		return nil, err
	}
	if setup.GraphSHA256 != s.GraphSHA256 {
		return nil, errors.New("the realm's graph changed since the run started, so its positions no longer hold; start a new run")
	}
	if setup.BriefSHA256 != s.BriefSHA256 {
		return nil, errors.New("the brief changed since the run started; a brief is frozen, so start a new run")
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), s.Ledger)
	if err != nil {
		return nil, err
	}
	r := &Run{Project: project, Dir: dir, State: s, Setup: setup, Ledger: l, unlock: func() {}}
	if setup.ConfigSHA256 != s.ConfigSHA256 {
		s.ConfigSHA256 = setup.ConfigSHA256
		if err := r.Log(gungnir.Event{Name: gungnir.EventConfigChanged, ConfigHash: setup.ConfigSHA256}); err != nil {
			l.Close()
			return nil, err
		}
	}
	if mode != s.Mode {
		looser := mode.Looser(s.Mode)
		s.Mode = mode
		if err := r.Log(gungnir.Event{Name: gungnir.EventModeChanged, Mode: mode, Looser: looser}); err != nil {
			l.Close()
			return nil, err
		}
	}
	return r, r.Save()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
