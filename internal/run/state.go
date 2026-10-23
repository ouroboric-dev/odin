// Package run holds a run's directory, its run.json, its lock, and the checks that a run may start
// or continue (spec §4.1, §4.3, §12).
package run

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/budget"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/lock"
)

// StateSchema names the run.json format.
const StateSchema = "ygg.run/v3" // v2: runs live in the instance tree (Phase 2); v3: a graph pack leads, not a realm (Voluspa)

// stateSchemaV2 is the format of a run that a realm led. It can't be resumed: its positions and prompts came
// from that realm, which is no longer the graph.
const stateSchemaV2 = "ygg.run/v2"

// Status is where a run stands.
type Status string

const (
	StatusRunning Status = "running" // a process is walking the graph, or died while it was
	StatusStopped Status = "stopped" // waiting at an approval point
	StatusFailed  Status = "failed"  // a node failed; resume re-runs it
	StatusPaused  Status = "paused"  // the provider's quota is used up; resume re-runs the node once it is back
	StatusDone    Status = "done"
)

// RealmRecord is a realm leaf as the run loaded it.
type RealmRecord struct {
	Version string `json:"version"`
	Dir     string `json:"dir"`
	SHA256  string `json:"sha256"` // of every file a run could read; a resume refuses a realm whose hash changed
}

// RoleRecord is the agent and model a role used.
type RoleRecord struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// State is run.json. Odin is its only writer.
type State struct {
	Schema          string                 `json:"schema"`
	ID              string                 `json:"id"`
	Status          Status                 `json:"status"`
	Mode            gungnir.Mode           `json:"mode"`
	Graph           string                 `json:"graph"`             // the graph pack's name
	GraphVersion    string                 `json:"graph_version"`     // the graph pack's version
	GraphDir        string                 `json:"graph_dir"`         // the graph pack's folder, absolute: a resume loads it from there
	GraphPackSHA256 string                 `json:"graph_pack_sha256"` // of every file of the graph pack a run could read
	GraphSHA256     string                 `json:"graph_sha256"`      // of graph.yaml
	Realms          map[string]RealmRecord `json:"realms,omitempty"`  // every realm of the run, by name
	Factory         string                 `json:"factory,omitempty"` // the build of ygg that started the run (version.Factory)
	ConfigPath      string                 `json:"config_path"`
	ConfigSHA256    string                 `json:"config_sha256"`
	Brief           string                 `json:"brief,omitempty"`
	BriefSHA256     string                 `json:"brief_sha256,omitempty"`
	Roles           map[string]RoleRecord  `json:"roles"`
	Root            string                 `json:"root"`                   // the tree's root: the project, or an umbrella above it
	ProjectPath     string                 `json:"project"`                // the project's path from the root
	Leaves          map[string]LeafRecord  `json:"leaves"`                 // leaf path from the project: the run's branch and checkpoint in it
	Position        int                    `json:"position"`               // index of the next node to run
	Attempts        map[string]int         `json:"attempts"`               // node id: attempts started
	Pending         string                 `json:"pending,omitempty"`      // the node waiting for a decision
	PendingGate     gungnir.Gate           `json:"pending_gate,omitempty"` // the approval point it waits at
	Reasons         map[string]string      `json:"reasons,omitempty"`      // node id: rejection reason for its next attempt
	Summaries       map[string]string      `json:"summaries,omitempty"`    // node id: its last result
	Waived          map[string]bool        `json:"waived,omitempty"`       // pointer node id: shrink-guard approved for its next move
	Judged          map[string]string      `json:"judged,omitempty"`       // pointer node id: the commit the pointer-contract gate judged, which the move must be to
	Skills          skills.Record          `json:"skills"`                 // every skill the graph names, with the set's hash (spec §5.1)
	Sandboxes       map[string]gungnir.Env `json:"sandboxes,omitempty"`    // leaf path: its sandbox, while one is up, so a leftover can be stopped
	Usage           gungnir.Usage          `json:"usage"`
	PendingSpend    []budget.Entry         `json:"pending_spend,omitempty"` // spends in Usage the journal couldn't take yet, oldest first
	Paused          *Pause                 `json:"paused,omitempty"`        // why the run is paused
	Warned          map[string]bool        `json:"warned,omitempty"`        // the budget warnings already given, so each comes once
	Loops           map[string]*LoopRecord `json:"loops,omitempty"`         // loop node id: where its loop stands
	Features        []schema.FeatureRecord `json:"features,omitempty"`      // the plan's tasks in the order they are built, once the tasks node has run
	PlanSource      string                 `json:"plan_source,omitempty"`   // the tasks.md they came from, relative to the project
	PlanSHA256      string                 `json:"plan_sha256,omitempty"`   // that file as Odin read it
	Expanded        map[string]bool        `json:"expanded,omitempty"`      // foreach node ids already expanded into the nodes of each task
	NewLeaves       map[string]bool        `json:"new_leaves,omitempty"`    // leaf paths this run's init creates: only these get the realm's template
	Returns         map[string]int         `json:"returns,omitempty"`       // node id: how many times its refusal went back into an earlier node
	Refusals        map[string]string      `json:"refusals,omitempty"`      // node id: what a later step refused, for the node's next attempt
	Decisions       []Decision             `json:"decisions,omitempty"`     // the notes of gate approvals, in the order given: every later session reads them
	Ledger          ledger.Anchor          `json:"ledger"`
	Error           string                 `json:"error,omitempty"`
	Exec            *ExecProcess           `json:"exec,omitempty"` // the exec node running now, if any
	Created         time.Time              `json:"created"`
	Updated         time.Time              `json:"updated"`
}

// Decision is the note an operator gave when approving a gate node (spec §7.2): the product owner's
// decision, which every later session reads.
type Decision struct {
	Gate string `json:"gate"`
	Note string `json:"note"`
}

// LoopRecord is where one loop node stands (spec §4.2): Jormungandr's state, which holds the attempt,
// the phase and every finding, and why the loop waits for a person, if it does.
type LoopRecord struct {
	State      jormungandr.State `json:"state"`
	Escalation *Escalation       `json:"escalation,omitempty"`
}

// Escalation is why a loop gave up (spec §7.3): see the Reason constants of Jormungandr.
type Escalation struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Pause is why a run waits for the provider (spec §12).
type Pause struct {
	Node   string    `json:"node"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// LeafRecord is where a run stands in one leaf (spec §4.3).
type LeafRecord struct {
	Branch     string `json:"branch"`     // the run's branch: ygg/<run id>
	Base       string `json:"base"`       // the leaf's main when the run started
	Checkpoint string `json:"checkpoint"` // the last checkpoint commit; resume resets the leaf to it
	// TaskBase is the leaf's main when the task working in it now started: a task given up after its
	// merge landed puts main back there (spec §7.3).
	TaskBase string `json:"task_base,omitempty"`
	// Realm and RealmVersion are the realm leaf that provides this leaf, as the run used it, from the leaf's
	// own pin.
	Realm        string `json:"realm,omitempty"`
	RealmVersion string `json:"realm_version,omitempty"`
}

var idPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// NewID returns a run id: the UTC time and six random hex digits, such as 20261015-142233-a1b2c3.
func NewID(now time.Time, random io.Reader) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b), nil
}

// NewIDNow returns a run id for the current time.
func NewIDNow() (string, error) { return NewID(time.Now(), rand.Reader) }

// ValidID reports whether id is a run id, so it can't name a path outside the runs directory.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Dir returns the run directory of id under project.
func Dir(project, id string) string { return filepath.Join(project, ".factory", "runs", id) }

// Save writes run.json atomically: a temporary file in the same directory, synced, then renamed
// over the old one, then the directory synced (spec §12).
func (s *State) Save(dir string) error {
	s.Schema = StateSchema
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "run.json.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "run.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// LoadState reads dir/run.json and refuses unknown fields or another schema.
func LoadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	var head struct {
		Schema string `json:"schema"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%s/run.json: %w", dir, err)
	}
	if head.Schema == stateSchemaV2 {
		return nil, fmt.Errorf("%s/run.json: run %s was led by a realm (schema %s), from before graphs, so it can't be resumed; start a new run with --graph", dir, head.ID, stateSchemaV2)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s/run.json: %w", dir, err)
	}
	if s.Schema != StateSchema {
		return nil, fmt.Errorf("%s/run.json: schema %q, want %q", dir, s.Schema, StateSchema)
	}
	if s.Exec != nil {
		if err := s.Exec.Check(); err != nil {
			return nil, fmt.Errorf("%s/run.json: %w", dir, err)
		}
	}
	// Empty maps aren't written (omitempty), so make them again: the engine writes to all three.
	if s.Attempts == nil {
		s.Attempts = map[string]int{}
	}
	if s.Reasons == nil {
		s.Reasons = map[string]string{}
	}
	if s.Summaries == nil {
		s.Summaries = map[string]string{}
	}
	if s.Waived == nil {
		s.Waived = map[string]bool{}
	}
	if s.Judged == nil {
		s.Judged = map[string]string{}
	}
	if s.Warned == nil {
		s.Warned = map[string]bool{}
	}
	if s.Loops == nil {
		s.Loops = map[string]*LoopRecord{}
	}
	if s.Expanded == nil {
		s.Expanded = map[string]bool{}
	}
	if s.NewLeaves == nil {
		s.NewLeaves = map[string]bool{}
	}
	if s.Returns == nil {
		s.Returns = map[string]int{}
	}
	if s.Refusals == nil {
		s.Refusals = map[string]string{}
	}
	if s.Leaves == nil {
		s.Leaves = map[string]LeafRecord{}
	}
	return &s, nil
}

// ExecProcess is an exec node's running command: its process group, and its leader's start time,
// so a later ygg can tell the group is still this node's after its own process died.
type ExecProcess struct {
	Node  string `json:"node"`
	PGID  int    `json:"pgid"`
	Start uint64 `json:"start"` // the leader's start time, in clock ticks since boot; 0 if unknown
}

// Check refuses an exec record whose process group could not be an exec node's: a group of 1 or less would
// make a kill signal every process the user owns (kill(-1)) or ygg's own group (kill(0)), and run.json is
// data that a person or another program may have written.
func (p *ExecProcess) Check() error {
	if p.PGID <= 1 {
		return fmt.Errorf("corrupt exec record: node %s has process group %d, which no exec node has; ygg will not signal it", p.Node, p.PGID)
	}
	return nil
}

// procStart returns when process pid started, in clock ticks since boot (field 22 of
// /proc/<pid>/stat), or 0 if the process doesn't exist.
func procStart(pid int) uint64 {
	fields := procStat(pid)
	if len(fields) < 20 {
		return 0
	}
	n, _ := strconv.ParseUint(fields[19], 10, 64)
	return n
}

// procStat returns the fields of /proc/<pid>/stat from field 3 (the state) on, or nil.
func procStat(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	// The command name (field 2) is in parentheses and may hold spaces; fields from 3 follow it.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(b[i+1:]))
}

// groupAlive reports whether any process of group pgid is still running. A zombie isn't: it has
// exited and only waits for its parent to reap it, which a container's PID 1 may never do.
func groupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	want := strconv.Itoa(pgid)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// From field 3: [0] is the state, [2] the process group.
		if f := procStat(pid); len(f) > 2 && f[0] != "Z" && f[2] == want {
			return true
		}
	}
	return false
}

// ProcStart returns when process pid started, for ExecProcess.Start.
func ProcStart(pid int) uint64 { return procStart(pid) }

// ErrInUse means another ygg process holds the run's lock.
var ErrInUse = errors.New("another ygg process is using this run")

// Lock takes dir/lock for this process. The kernel drops the lock when the process ends, so a
// killed run never stays locked.
func Lock(dir string) (unlock func(), err error) {
	unlock, err = lock.Take(filepath.Join(dir, "lock"))
	if errors.Is(err, lock.ErrHeld) {
		return nil, ErrInUse
	}
	return unlock, err
}

// Locked reports whether a process holds dir/lock. It never takes the lock, so a status check
// can't make another process's resume fail.
func Locked(dir string) bool { return lock.Held(filepath.Join(dir, "lock")) }

// checkRealms refuses a resume when a realm changed under the run, or the project now names a realm the run
// did not load: its graph positions and prompts would no longer mean what they meant.
func (s *State) checkRealms(setup *Setup) error {
	for _, name := range setup.Realms.Names() {
		got := setup.Realms.Realms[name]
		want, ok := s.Realms[name]
		switch {
		case !ok:
			return fmt.Errorf("the project now names the realm %s, which this run didn't start with; start a new run", name)
		case got.Version != want.Version:
			return fmt.Errorf("the realm %s is at %s, and the run started with %s; start a new run", name, got.Version, want.Version)
		case got.Hash != want.SHA256:
			return fmt.Errorf("the realm %s changed since the run started (%s; it was %s and is now %s), so the run's prompts and checks are not the ones it began with; put the realm back as it was and the run resumes and can be graded, or start a new run",
				name, want.Dir, shortHash(want.SHA256), shortHash(got.Hash))
		}
	}
	return nil
}

// shortHash is the first 12 characters of a hash, enough to tell two realm states apart in a message.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// check refuses a run.json whose position, pending node or leaves don't fit setup's graph and
// tree. Odin wrote it, but a hand-edited file must not crash the engine.
func (s *State) check(setup *Setup) error {
	nodes, err := setup.NodesFor(s)
	if err != nil {
		return fmt.Errorf("the tasks the run expanded no longer fit the project: %w", err)
	}
	if s.Position < 0 || s.Position > len(nodes) {
		return fmt.Errorf("position %d is outside the graph's %d nodes", s.Position, len(nodes))
	}
	if s.Pending != "" && (s.Position >= len(nodes) || nodes[s.Position].ID != s.Pending) {
		return fmt.Errorf("pending %q is not the node at position %d", s.Pending, s.Position)
	}
	for p := range s.Leaves {
		if !slices.Contains(setup.Leaves(), p) {
			return fmt.Errorf("leaf %s is not one the graph works in", p)
		}
	}
	return nil
}

// BeforeGraphs says whether the run folder holds a run of schema ygg.run/v2, which a realm led and
// which can't be resumed. It reads only the run's exec node, which a v2 run.json had in the
// form v3 has, so doctor can stop a leftover process without v2's other fields.
func BeforeGraphs(dir string) (exec *ExecProcess, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, false
	}
	var head struct {
		Schema string       `json:"schema"`
		Exec   *ExecProcess `json:"exec"`
	}
	if json.Unmarshal(data, &head) != nil || head.Schema != stateSchemaV2 {
		return nil, false
	}
	return head.Exec, true
}
