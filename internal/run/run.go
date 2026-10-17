package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/agents"
	"github.com/ouroboric-dev/odin/internal/budget"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/ledger"
	"github.com/ouroboric-dev/odin/internal/lock"
	"github.com/ouroboric-dev/odin/internal/plan"
	"github.com/ouroboric-dev/odin/internal/realm"
	"github.com/ouroboric-dev/odin/internal/remote"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// Options say what to run.
type Options struct {
	Project string            // the project umbrella (L1), inside an instance tree
	Config  string            // factory.yaml
	Realm   string            // the lead realm: a realm leaf's name, found in factory.yaml's realms.dir, or a path to the folder that holds pack.yaml
	Realms  map[string]string // a resume: the folder of each realm the run recorded, by name; they are loaded from there
	Leaves  []string          // a resume: the leaves the run added beyond the ones its graph names (AddLeaf)
	Brief   string            // optional
	Mode    gungnir.Mode      // "" means the config's mode
	Hooks   Hooks             // tests only
}

// Hooks replace parts of a run in tests: the skill libraries, the sandbox driver, the clock that
// decides which day a session's spend belongs to, and the retry policy with its wait.
type Hooks struct {
	Libs       *skills.Libraries
	NewSandbox NewSandbox
	Now        func() time.Time
	Backoff    *jormungandr.Backoff
	Sleep      func(ctx context.Context, d time.Duration) error
}

// Setup is everything a run needs, loaded and checked.
type Setup struct {
	Config       *schema.Factory
	ConfigPath   string // absolute
	ConfigSHA256 string
	Pack         *schema.Pack // the lead realm's
	RealmDir     string       // the lead realm's folder, absolute
	Lead         *realm.Realm // the lead realm: --realm, loaded
	Realms       *realm.Set   // every realm of the run: the lead's and one for each realm a leaf of the project names, each loaded once
	Graph        *schema.Graph
	GraphSHA256  string
	Brief        string // absolute, or ""
	BriefText    string
	BriefSHA256  string
	Mode         gungnir.Mode
	Agents       map[string]gungnir.Agent // role: adapter, for every role the graph uses
	Libs         skills.Libraries         // where the graph's skills come from
	Skills       skills.Record            // every skill the graph names, with the set's hash
	Sandbox      *SandboxSetup            // nil: no sandbox section, so leaf commands run on the host
	newSandbox   NewSandbox
	Journal      *budget.Journal // where every session's usage is recorded, and the per-day cap is summed
	Backoff      jormungandr.Backoff
	Sleep        func(ctx context.Context, d time.Duration) error // nil: wait for real
	PinLead      bool                                             // the lead realm was named, and the project doesn't pin one yet: Start pins it

	Root        string               // the tree's root: the project, or an umbrella above it
	ProjectPath string               // the project's path from the root
	Project     tree.Node            // the project umbrella
	Repos       map[string]tree.Node // every repo the graph names, by path from the project; "." is the project
	Create      []string             // leaves the project lacks, which Start creates from the realm's templates
}

// Prepare loads and checks the config, the realm, the brief and the project's place in the
// instance tree, and builds the agents. It writes nothing, and checks out only the submodules on
// the paths to the project and its repos. Incomplete caps give a *schema.CapsError (change
// require-budget-caps).
func Prepare(ctx context.Context, o Options) (*Setup, error) {
	s := &Setup{Agents: map[string]gungnir.Agent{}, Repos: map[string]tree.Node{}}
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
	// ParseFactory checked the gate overrides against the file's own mode. A --mode switch needs no
	// second check: design: auto, the one loosening, applies only in unattended mode (gate.Decide).
	s.Mode = s.Config.Mode
	if o.Mode != "" {
		s.Mode = o.Mode
	}
	var store *realm.Store
	if s.Config.Realms != nil {
		store = &realm.Store{Dir: s.Config.Realms.Dir}
	}
	realmDir, byName, err := realm.Resolve(o.Realm, store)
	if err != nil {
		return nil, err
	}
	if s.Lead, err = realm.Load(realmDir); err != nil {
		return nil, err
	}
	s.Pack, s.RealmDir = s.Lead.Pack, s.Lead.Dir
	if s.Pack.Graph == "" {
		return nil, fmt.Errorf("the realm %s has no graph, so it can't lead a run; --realm names the realm whose graph builds the product", s.Pack.Name)
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
	if err := s.findProject(ctx, o.Project); err != nil {
		return nil, err
	}
	if s.Realms, err = s.collectRealms(store, o.Realms); err != nil {
		return nil, err
	}
	for _, p := range o.Leaves {
		if err := s.addLeaf(ctx, p); err != nil {
			return nil, err
		}
	}
	// A realm named in the store is pinned by the project's first run; one given as a path is a checkout
	// being worked on, and pins nothing.
	s.PinLead = byName && s.Project.Umbrella != nil && s.Project.Umbrella.Realm == ""
	for _, n := range s.Graph.Nodes {
		if err := s.checkNode(ctx, graphPath, n); err != nil {
			return nil, err
		}
	}
	if err := s.prepareBudget(o.Hooks); err != nil {
		return nil, err
	}
	s.newSandbox = o.Hooks.NewSandbox
	if err := s.prepareSandbox(); err != nil {
		return nil, err
	}
	if err := s.prepareSkills(o.Hooks.Libs); err != nil {
		return nil, err
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

// prepareBudget says where the spend journal is, and the retry policy. The journal's folder is made
// at the first session, not here: Prepare writes nothing.
func (s *Setup) prepareBudget(h Hooks) error {
	var err error
	if s.Journal, err = BudgetJournal(s.Config, h.Now); err != nil {
		return fmt.Errorf("%s: %w", s.ConfigPath, err)
	}
	s.Backoff, s.Sleep = jormungandr.DefaultBackoff, h.Sleep
	if h.Backoff != nil {
		s.Backoff = *h.Backoff
	}
	return nil
}

// BudgetJournal returns the spend journal of a config: the folder it names, or the operator's state
// folder. now is the clock of a test; nil is the real one.
func BudgetJournal(cfg *schema.Factory, now func() time.Time) (*budget.Journal, error) {
	dir := cfg.Budget.Journal
	if dir == "" {
		var err error
		if dir, err = budget.DefaultDir(os.Getenv, os.UserHomeDir); err != nil {
			return nil, err
		}
	}
	return &budget.Journal{Dir: dir, Now: now}, nil
}

// findProject places the project in the instance tree: Odin runs on nothing outside it, because a
// resume resets leaves (spec §4.3).
func (s *Setup) findProject(ctx context.Context, project string) error {
	var err error
	if s.Root, s.ProjectPath, err = tree.FindRoot(ctx, project); err != nil {
		return err
	}
	nodes, err := tree.Resolve(ctx, s.Root, s.ProjectPath)
	if err != nil {
		return err
	}
	s.Project = nodes[len(nodes)-1]
	if s.Project.Kind != schema.ChildUmbrella {
		return fmt.Errorf("%s is a leaf; --project names a project umbrella", project)
	}
	s.Repos["."] = s.Project
	return nil
}

// checkNode checks that node n can run in this project: its repo, its role and prompt, its checks.
func (s *Setup) checkNode(ctx context.Context, graphPath string, n schema.Node) error {
	switch {
	case n.Kind == schema.KindGate, n.Kind == schema.KindPublish, n.Kind == schema.KindTasks, n.Kind == schema.KindInit:
		return nil // they work in the project, not in a leaf
	case n.Kind == schema.KindForeach:
		return s.checkBody(graphPath, n)
	}
	if n.Repo == "" {
		return fmt.Errorf("%s: node %s names no repo; every node that does work runs in a repo of the project", graphPath, n.ID)
	}
	if _, ok := s.Repos[n.Repo]; !ok && !slices.Contains(s.Create, n.Repo) {
		nodes, err := tree.Resolve(ctx, s.Root, filepath.Join(s.ProjectPath, n.Repo))
		switch _, creatable := s.Pack.Leaves[n.Repo]; {
		case err == nil:
			s.Repos[n.Repo] = nodes[len(nodes)-1]
		case creatable:
			s.Create = append(s.Create, n.Repo)
		default:
			return fmt.Errorf("%s: node %s works in %s, which the project lacks and the realm doesn't create: %w", graphPath, n.ID, n.Repo, err)
		}
	}
	if n.Kind != schema.KindExec && !s.isLeaf(n.Repo) {
		return fmt.Errorf("%s: node %s works in %s, which is an umbrella; agents, merges and pointer moves work in a leaf (spec §3.3)", graphPath, n.ID, n.Repo)
	}
	for _, c := range slices.Concat(n.Checks, n.Acceptance) {
		if _, ok := s.Pack.Checks[c]; !ok {
			return fmt.Errorf("%s: node %s runs check %s, which the pack doesn't define", graphPath, n.ID, c)
		}
	}
	if n.Kind != schema.KindAgent && n.Kind != schema.KindLoop {
		return nil
	}
	if err := s.checkSession(graphPath, n.ID, n.Role, n.Prompt); err != nil {
		return err
	}
	for _, rv := range n.Reviewers {
		if err := s.checkSession(graphPath, n.SessionKey(rv), rv.Role, rv.Prompt); err != nil {
			return err
		}
	}
	return nil
}

// checkBody checks the roles and prompts of a foreach node's body, which run in leaves the project
// decides later: a role must be in factory.yaml, a reviewer's prompt must be in the lead realm, and the
// prompt and checks of a leaf's loop come from the realm of the leaf, where Expand finds them.
func (s *Setup) checkBody(graphPath string, fe schema.Node) error {
	for _, n := range fe.Body {
		switch n.Kind {
		case schema.KindAgent:
			if err := s.checkSession(graphPath, n.ID, n.Role, n.Prompt); err != nil {
				return err
			}
		case schema.KindLoop:
			if _, ok := s.Config.Roles[n.Role]; !ok {
				return fmt.Errorf("%s: node %s uses role %s, which %s doesn't define", graphPath, n.ID, n.Role, s.ConfigPath)
			}
			if s.Agents[n.Role] == nil {
				a, err := agents.New(s.Config.Roles[n.Role], filepath.Dir(s.ConfigPath), agents.Options{Privacy: s.Config.Privacy})
				if err != nil {
					return fmt.Errorf("role %s: %w", n.Role, err)
				}
				s.Agents[n.Role] = a
			}
			for _, rv := range n.Reviewers {
				if err := s.checkSession(graphPath, fe.ID+"-"+n.ID+"-"+rv.ID, rv.Role, rv.Prompt); err != nil {
					return err
				}
			}
		}
		for _, c := range n.Acceptance {
			if _, ok := s.Pack.Checks[c]; !ok {
				return fmt.Errorf("%s: node %s runs check %s, which the pack doesn't define", graphPath, n.ID, c)
			}
		}
	}
	return nil
}

// checkSession checks that a session of node (a node, or a loop's reviewer) can run: its prompt file
// is in the realm and its role is in factory.yaml, whose adapter is built once per role.
func (s *Setup) checkSession(graphPath, node, role, prompt string) error {
	if _, err := os.Stat(filepath.Join(s.RealmDir, prompt)); err != nil {
		return fmt.Errorf("%s: node %s: %w", graphPath, node, err)
	}
	if s.Agents[role] != nil {
		return nil
	}
	r, ok := s.Config.Roles[role]
	if !ok {
		return fmt.Errorf("%s: node %s uses role %s, which %s doesn't define", graphPath, node, role, s.ConfigPath)
	}
	var err error
	if s.Agents[role], err = agents.New(r, filepath.Dir(s.ConfigPath), agents.Options{Privacy: s.Config.Privacy}); err != nil {
		return fmt.Errorf("role %s: %w", role, err)
	}
	return nil
}

// collectRealms loads the realms of the run: the lead's, and one for each realm the project's pins name.
// The pins must agree with each other and with what the realm store holds (change realm-leaf-naming), so
// a mismatch stops the run before anything is created or started. recorded holds the folders a resumed
// run loaded its realms from.
func (s *Setup) collectRealms(store *realm.Store, recorded map[string]string) (*realm.Set, error) {
	var pins []schema.Pin
	if u := s.Project.Umbrella; u != nil {
		for _, p := range u.Pins() {
			if p.Where == "." || u.Children[p.Where].Kind == schema.ChildLeaf {
				pins = append(pins, p)
			}
		}
	}
	return realm.Collect(s.Lead, pins, func(name string) (*realm.Realm, error) {
		if dir, ok := recorded[name]; ok {
			return realm.Load(dir)
		}
		if store == nil {
			return nil, errors.New("factory.yaml has no realms section to find it in")
		}
		dir, err := store.Find(name)
		if err != nil {
			return nil, err
		}
		return realm.Load(dir)
	})
}

// Realm returns the realm called name, or the lead realm for "".
func (s *Setup) Realm(name string) *realm.Realm {
	if name == "" {
		return s.Lead
	}
	return s.Realms.Realms[name]
}

// RealmOf returns the realm that provides the leaf at repo: the one the project pins for it, or the lead
// realm when the leaf has no pin.
func (s *Setup) RealmOf(repo string) *realm.Realm {
	if u := s.Project.Umbrella; u != nil {
		if c, ok := u.Children[repo]; ok && c.Realm != "" {
			if r := s.Realms.Realms[c.Realm]; r != nil {
				return r
			}
		}
	}
	return s.Lead
}

// isLeaf reports whether repo is a leaf: one the tree records as a leaf, or one Start creates.
func (s *Setup) isLeaf(repo string) bool {
	if n, ok := s.Repos[repo]; ok {
		return n.Kind == schema.ChildLeaf
	}
	return slices.Contains(s.Create, repo)
}

// addLeaf resolves the leaf at repo, a path from the project, and makes it one of the run's repos.
func (s *Setup) addLeaf(ctx context.Context, repo string) error {
	if !filepath.IsLocal(repo) || repo == "." {
		return fmt.Errorf("%q is not a path inside the project", repo)
	}
	if _, ok := s.Repos[repo]; ok {
		return nil
	}
	if u := s.Project.Umbrella; u == nil || u.Children[repo].Kind != schema.ChildLeaf {
		return fmt.Errorf("%s is not a leaf of the project", repo)
	}
	nodes, err := tree.Resolve(ctx, s.Root, filepath.Join(s.ProjectPath, repo))
	if err != nil {
		return err
	}
	s.Repos[repo] = nodes[len(nodes)-1]
	if s.Sandbox != nil {
		if _, _, _, err := s.SandboxFor(repo); err != nil {
			delete(s.Repos, repo)
			return err
		}
	}
	return nil
}

// AddLeaf makes the leaf at repo part of the run once the run knows it needs it: a task works in it,
// or init made it. The leaf gets the run's branch and a record in run.json, as a leaf its graph names
// does at the start; a leaf the run already has is left alone.
func (r *Run) AddLeaf(ctx context.Context, repo string) error {
	if err := r.Setup.addLeaf(ctx, repo); err != nil {
		return err
	}
	if _, ok := r.State.Leaves[repo]; ok {
		return nil
	}
	l, err := r.Leaf(repo)
	if err != nil {
		return err
	}
	head, err := l.StartBranch(ctx, Branch(r.State.ID), "main")
	if err != nil {
		return err
	}
	r.State.Leaves[repo] = LeafRecord{Branch: Branch(r.State.ID), Base: head, Checkpoint: head}
	return r.Save()
}

// Leaves returns the paths of the leaves the graph works in, sorted.
func (s *Setup) Leaves() []string {
	var ls []string
	for p, n := range s.Repos {
		if n.Kind == schema.ChildLeaf {
			ls = append(ls, p)
		}
	}
	slices.Sort(ls)
	return ls
}

// Run is an open run: its directory, state, setup, ledger and locks.
type Run struct {
	Project  string
	Dir      string
	State    *State
	Setup    *Setup
	Ledger   *ledger.Ledger
	unlock   func()
	changes  []gungnir.Event // a mode or config switch Open found, not yet in the ledger
	Saved    []string        // one note per leaf whose discarded changes Open saved to a ref, or discarded unsaved, before resetting it
	SetAside []string        // the notes of Saved from the set-aside of a session whose spend the journal couldn't take, for its gate (review I4)
	Sandbox  gungnir.Sandbox // runs the leaves' commands; nil when the config has no sandbox section
	nodes    []schema.Node   // the nodes of the run, with foreach nodes expanded; see Nodes
}

// Nodes returns the nodes the run walks, in order: the graph's, with each foreach node that has been
// expanded replaced by the nodes of its tasks. Position indexes this list.
func (r *Run) Nodes() []schema.Node {
	if r.nodes == nil {
		r.nodes = r.Setup.Graph.Nodes
	}
	return r.nodes
}

// Expand replaces foreach node id by its task shape for each task of the plan, in the leaf and the realm
// each task names, and every leaf the tasks work in becomes part of the run. Position is not changed: it
// then points at the first node of the first task's shape. The expansion is recorded in run.json, so a
// resume makes the same nodes.
func (r *Run) Expand(ctx context.Context, id string) error {
	s := r.State
	for _, f := range s.Features {
		if err := r.AddLeaf(ctx, f.Layer); err != nil {
			return fmt.Errorf("task %s works in %s: %w", f.ID, f.Layer, err)
		}
	}
	s.Expanded[id] = true
	nodes, err := r.Setup.NodesFor(s)
	if err != nil {
		delete(s.Expanded, id)
		return err
	}
	r.nodes = nodes
	return r.Save()
}

// NodesFor returns the nodes of the run in the state st: the graph's, with each foreach node that st has
// expanded replaced by its task shape once for each task, in the order the tasks are built.
func (s *Setup) NodesFor(st *State) ([]schema.Node, error) {
	var out []schema.Node
	for _, n := range s.Graph.Nodes {
		if n.Kind != schema.KindForeach || !st.Expanded[n.ID] {
			out = append(out, n)
			continue
		}
		fs := make([]schema.Feature, len(st.Features))
		for i, f := range st.Features {
			fs[i] = f.Feature
		}
		ex, err := plan.Expand(n, fs, s.Graph, s.recipeOf)
		if err != nil {
			return nil, err
		}
		out = append(out, ex...)
	}
	return out, nil
}

// recipeOf says what the realm of the leaf at layer offers its tasks.
func (s *Setup) recipeOf(layer string) (plan.Recipe, error) {
	if u := s.Project.Umbrella; u == nil || u.Children[layer].Kind != schema.ChildLeaf {
		return plan.Recipe{}, fmt.Errorf("the project has no leaf %s", layer)
	}
	r := s.RealmOf(layer)
	rec := plan.Recipe{Realm: r.Name}
	if r.Pack.Leaf != nil {
		rec.Implementer, rec.Checks = r.Pack.Leaf.Implementer, r.Pack.Leaf.Checks
	}
	return rec, nil
}

// Meter returns the budget of this run: its caps against its own usage and the day's journal.
func (r *Run) Meter() *budget.Meter {
	return &budget.Meter{Run: &r.State.Usage, Caps: r.Setup.Config.Budget, USD: r.Setup.Config.PaysPerToken(), Journal: r.Setup.Journal}
}

// Spend counts what a session used, whether or not it succeeded: in the day's journal first, so a
// crash between the two leaves the spend counted twice at worst, and then in the run's usage. The
// caller saves run.json. The spends the journal missed before are written first, in order. A journal
// that can't take the line is a per-day cap that is reached (gungnir.CapJournal), joined with a failed
// save, and the run's usage counts the spend all the same; the entry is kept in run.json as pending,
// saved here, since the caller stops on the cap, and the next Spend or Open writes it (review I4). A
// usage of nothing is not recorded.
func (r *Run) Spend(node string, u gungnir.Usage) error {
	jerr, serr := r.appendPending(r.Save)
	if u != (gungnir.Usage{}) {
		e := r.Setup.Journal.Stamp(budget.Entry{Run: r.State.ID, Project: r.Project, Node: node, Usage: u})
		// Behind the pending ones, if any are left, so the journal keeps the order.
		waits := len(r.State.PendingSpend) > 0
		if !waits {
			jerr = r.Setup.Journal.Append(e)
		}
		r.State.Usage = r.State.Usage.Add(u)
		if waits || jerr != nil {
			r.State.PendingSpend = append(r.State.PendingSpend, e)
			serr = errors.Join(serr, r.Save())
		}
	}
	return errors.Join(r.journalError(jerr), serr)
}

// appendPending writes the spends the journal missed, oldest first, and drops each from run.json
// once it is written, saving run.json with save. It stops at the first the journal can't take, which
// stays pending: jerr is the journal's error, serr a failed save.
func (r *Run) appendPending(save func() error) (jerr, serr error) {
	for len(r.State.PendingSpend) > 0 {
		e := r.State.PendingSpend[0]
		// The journal may hold it already: the process can stop between the append and the save.
		has, err := r.Setup.Journal.Has(e)
		if err == nil && !has {
			err = r.Setup.Journal.Append(e)
		}
		if err != nil {
			return err, nil
		}
		r.State.PendingSpend = r.State.PendingSpend[1:]
		if err := save(); err != nil {
			return nil, err
		}
	}
	r.State.PendingSpend = nil
	return nil, nil
}

// journalError is a journal that couldn't take a spend: a cap, since the per-day cap can't be kept
// without the day's spend; nil stays nil.
func (r *Run) journalError(err error) error {
	if err == nil {
		return nil
	}
	return &gungnir.CapError{Scope: gungnir.CapDay, Dimension: gungnir.CapJournal, Note: err.Error()}
}

// Branch is the branch a run works on in every leaf.
func Branch(id string) string { return "ygg/" + id }

// RepoDir is the working copy of a repo the graph names.
func (r *Run) RepoDir(repo string) string { return r.Setup.Repos[repo].Dir }

// Leaf returns a leaf the graph works in.
func (r *Run) Leaf(repo string) (*leaf.Leaf, error) { return leaf.Open(r.Setup.Repos[repo]) }

// Home is the HOME of exec nodes: the run's own directory, never a repo, so caches don't end up
// in a commit.
func (r *Run) Home() string { return filepath.Join(r.Dir, "home") }

// LockFile returns the lock file of a repo of the project, in <project>/.factory/locks/.
func (r *Run) LockFile(repo string) string { return LockFile(r.Project, repo) }

// LockFile returns the lock file of a repo of project: the merge gate's for a leaf, the pointer
// move's for an umbrella ("." is the project umbrella).
func LockFile(project, repo string) string {
	name := "project"
	if repo != "." {
		name = "repo-" + filepath.ToSlash(repo)
	}
	return filepath.Join(project, ".factory", "locks", filepath.FromSlash(name)+".lock")
}

// RunsLock returns the lock that one advancing run of project holds. It is not the project
// umbrella's own lock, which a pointer move into the umbrella takes while the run holds this one.
func RunsLock(project string) string { return filepath.Join(project, ".factory", "locks", "runs.lock") }

// ErrProjectBusy means another ygg process is advancing a run of the same project. One run at a
// time walks a project's leaves, which share their working copies.
var ErrProjectBusy = errors.New("another ygg process is advancing a run of this project")

// Log appends e to the ledger, with the run's id. A switch Open found is logged first.
func (r *Run) Log(e gungnir.Event) error {
	if err := r.logChanges(); err != nil {
		return err
	}
	e.Run = r.State.ID
	return r.Ledger.Append(e)
}

// logChanges logs the switches Open found. They wait until the command writes something, so a
// command that is refused leaves run.json and the ledger as they were (exit 2: nothing ran).
func (r *Run) logChanges() error {
	for len(r.changes) > 0 {
		e := r.changes[0]
		e.Run = r.State.ID
		if err := r.Ledger.Append(e); err != nil {
			return err
		}
		r.changes = r.changes[1:]
	}
	return nil
}

// Save records the ledger's head in run.json and writes it, after logging any switch Open found.
func (r *Run) Save() error {
	if err := r.logChanges(); err != nil {
		return err
	}
	r.State.Ledger = r.Ledger.Head()
	r.State.Updated = time.Now().UTC()
	return r.State.Save(r.Dir)
}

// Close closes the ledger and releases the locks.
func (r *Run) Close() error {
	err := r.Ledger.Close()
	r.unlock()
	return err
}

// takeLocks takes the run's lock, then the project's, and returns the function that releases
// both. Neither waits, so the order can't deadlock; run first names the precise conflict.
func takeLocks(project, dir string) (func(), error) {
	releaseRun, err := Lock(dir)
	if err != nil {
		return nil, err
	}
	releaseProject, err := lock.Take(RunsLock(project))
	if err != nil {
		releaseRun()
		if errors.Is(err, lock.ErrHeld) {
			return nil, ErrProjectBusy
		}
		return nil, err
	}
	return func() { releaseProject(); releaseRun() }, nil
}

// Failed is a run that started and then failed before its first node; run.json records why.
type Failed struct {
	ID  string
	Err error
}

func (f *Failed) Error() string { return fmt.Sprintf("run %s failed: %v", f.ID, f.Err) }
func (f *Failed) Unwrap() error { return f.Err }

// Start prepares a new run, then creates its directory, run.json and first ledger line, creates
// the leaves the project lacks, and starts the run's branch in every leaf. If Prepare refuses, or
// the run can't be recorded, nothing is left behind. A failure after that is a *Failed.
func Start(ctx context.Context, o Options) (*Run, error) {
	setup, err := Prepare(ctx, o)
	if err != nil {
		return nil, err
	}
	project := setup.Project.Dir
	id, err := NewIDNow()
	if err != nil {
		return nil, err
	}
	dir := Dir(project, id)
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		return nil, err
	}
	r, err := record(project, dir, id, setup)
	if err != nil {
		os.RemoveAll(dir) // the run's own new directory: nothing ran
		return nil, err
	}
	if err := r.pinLead(ctx); err != nil {
		r.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := r.openSandbox(setup.newSandbox); err != nil {
		r.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := r.begin(ctx); err != nil {
		r.State.Status, r.State.Error = StatusFailed, err.Error()
		r.Log(gungnir.Event{Name: gungnir.EventRunFailed, Result: r.State.Error})
		r.Save()
		r.Close()
		return nil, &Failed{ID: id, Err: err}
	}
	return r, r.Save()
}

// pinLead records the lead realm and its version in the project's umbrella.yaml, in a commit of its own,
// when the project pins none: from then on every run of the project must agree with it. The operator's
// own uncommitted edit of umbrella.yaml is not Odin's to commit.
func (r *Run) pinLead(ctx context.Context) error {
	s := r.Setup
	if !s.PinLead {
		return nil
	}
	p := s.Project
	if st, err := git.Run(ctx, p.Dir, "status", "--porcelain", "--", schema.UmbrellaFile); err != nil || st != "" {
		return fmt.Errorf("%s has changes that are not committed, so the run can't pin the realm %s %s in it: commit or discard them first (%v)",
			filepath.Join(p.Dir, schema.UmbrellaFile), s.Lead.Name, s.Lead.Version, err)
	}
	p.Umbrella.Realm, p.Umbrella.RealmVersion = s.Lead.Name, s.Lead.Version
	if err := p.Umbrella.Save(p.Dir); err != nil {
		return err
	}
	if _, err := git.Run(ctx, p.Dir, "add", "--", schema.UmbrellaFile); err != nil {
		return err
	}
	_, err := git.Run(ctx, p.Dir, "commit", "-q", "-m", fmt.Sprintf("Pin the lead realm %s %s", s.Lead.Name, s.Lead.Version), "--", schema.UmbrellaFile)
	return err
}

// realmNote lists the realms of the run for the ledger's first line: each with its version and the hash
// a resume compares.
func realmNote(set *realm.Set) string {
	var parts []string
	for _, name := range set.Names() {
		r := set.Realms[name]
		parts = append(parts, fmt.Sprintf("%s %s sha256:%s", r.Name, r.Version, r.Hash))
	}
	return "realms: " + strings.Join(parts, "; ")
}

// record locks the new run and writes its run.json and first ledger line.
func record(project, dir, id string, setup *Setup) (*Run, error) {
	unlock, err := takeLocks(project, dir)
	if err != nil {
		return nil, err
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), ledger.Anchor{})
	if err != nil {
		unlock()
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{Project: project, Dir: dir, Setup: setup, Ledger: l, unlock: unlock, State: &State{
		ID: id, Status: StatusRunning, Mode: setup.Mode,
		Realm: setup.Pack.Name, RealmVersion: setup.Pack.Version, RealmDir: setup.RealmDir, Realms: realmRecords(setup),
		GraphVersion: setup.Graph.Version, GraphSHA256: setup.GraphSHA256,
		ConfigPath: setup.ConfigPath, ConfigSHA256: setup.ConfigSHA256,
		Brief: setup.Brief, BriefSHA256: setup.BriefSHA256, Roles: roles(setup), Skills: setup.Skills,
		Root: setup.Root, ProjectPath: setup.ProjectPath, Leaves: map[string]LeafRecord{},
		Attempts: map[string]int{}, Reasons: map[string]string{}, Summaries: map[string]string{},
		Waived: map[string]bool{}, Warned: map[string]bool{}, Loops: map[string]*LoopRecord{}, Expanded: map[string]bool{}, Returns: map[string]int{}, Refusals: map[string]string{}, Created: now, Updated: now,
	}}
	started := gungnir.Event{Name: gungnir.EventRunStarted, Mode: setup.Mode, ConfigHash: setup.ConfigSHA256,
		Realm: setup.Lead.Name, RealmVersion: setup.Lead.Version, Note: realmNote(setup.Realms)}
	if err := r.Log(started); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Save(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// realmRecords is what run.json keeps of each realm of the run, so a resume loads the same folders and
// can tell none of them changed.
func realmRecords(setup *Setup) map[string]RealmRecord {
	out := map[string]RealmRecord{}
	for _, name := range setup.Realms.Names() {
		r := setup.Realms.Realms[name]
		out[name] = RealmRecord{Version: r.Version, Dir: r.Dir, SHA256: r.Hash}
	}
	return out
}

func roles(setup *Setup) map[string]RoleRecord {
	roles := map[string]RoleRecord{}
	for name := range setup.Agents {
		roles[name] = RoleRecord{Agent: setup.Config.Roles[name].Agent, Model: setup.Config.Roles[name].Model}
	}
	return roles
}

// begin creates the leaves the project lacks and starts the run's branch in every leaf that has no
// record yet. Start runs it, and Open again, so a run whose start failed resumes once the cause is
// fixed.
func (r *Run) begin(ctx context.Context) error {
	s := r.Setup
	for _, p := range s.Create {
		if err := r.createLeaf(ctx, p); err != nil {
			return err
		}
	}
	for _, p := range s.Leaves() {
		if _, ok := r.State.Leaves[p]; ok {
			continue // started already: a resume of a run whose start failed after this leaf
		}
		l, err := r.Leaf(p)
		if err != nil {
			return err
		}
		head, err := l.StartBranch(ctx, Branch(r.State.ID), "main")
		if err != nil {
			return err
		}
		r.State.Leaves[p] = LeafRecord{Branch: Branch(r.State.ID), Base: head, Checkpoint: head}
	}
	return nil
}

// createLeaf creates leaf p in the project through the remote, from the realm's template (spec
// §4.1; change remote-creates-repos).
func (r *Run) createLeaf(ctx context.Context, p string) error {
	s := r.Setup
	pl := s.Pack.Leaves[p]
	files := map[string][]byte{}
	for _, f := range pl.FirstCommit {
		b, err := os.ReadFile(filepath.Join(s.RealmDir, pl.Template, f))
		if err != nil {
			return err
		}
		files[f] = b
	}
	repo := gungnir.Repo{Owner: s.Project.Owner, Name: filepath.Base(p), Visibility: s.Project.Visibility,
		Description: pl.Description, Path: p, Files: files}
	rem := remote.NewLocal(s.Project)
	if err := rem.Create(ctx, repo); err != nil {
		return err
	}
	nodes, err := tree.Resolve(ctx, s.Root, filepath.Join(s.ProjectPath, p))
	if err != nil {
		return err
	}
	s.Repos[p] = nodes[len(nodes)-1]
	s.Create = slices.DeleteFunc(s.Create, func(c string) bool { return c == p })
	return r.Log(gungnir.Event{Name: gungnir.EventRepoCreated, Note: fmt.Sprintf("%s (%s remote)", p, rem.Name())})
}

// Open locks and loads run id for resume, approve or reject, and checks that it may continue: the
// caps are still complete, the graph and brief are unchanged, the ledger reaches the anchor in
// run.json, and every leaf is at its checkpoint. A run that was interrupted or failed has its
// leaves reset to their checkpoints, since its node restarts from scratch (spec §4.3). mode
// switches the run's mode at this pause ("" keeps it); a switch is logged.
func Open(ctx context.Context, project, id string, mode gungnir.Mode) (*Run, error) {
	return OpenWith(ctx, project, id, mode, Hooks{})
}

// OpenWith is Open with parts replaced, for tests.
func OpenWith(ctx context.Context, project, id string, mode gungnir.Mode, h Hooks) (*Run, error) {
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
	unlock, err := takeLocks(project, dir)
	if err != nil {
		return nil, err
	}
	r, err := open(ctx, project, dir, mode, h)
	if err != nil {
		unlock()
		return nil, err
	}
	r.unlock = unlock
	return r, nil
}

func open(ctx context.Context, project, dir string, mode gungnir.Mode, h Hooks) (*Run, error) {
	s, err := LoadState(dir)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = s.Mode
	}
	dirs := map[string]string{}
	for name, r := range s.Realms {
		dirs[name] = r.Dir
	}
	setup, err := Prepare(ctx, Options{Project: project, Config: s.ConfigPath, Realm: s.RealmDir, Realms: dirs, Leaves: slices.Sorted(maps.Keys(s.Leaves)),
		Brief: s.Brief, Mode: mode, Hooks: h})
	if err != nil {
		return nil, err
	}
	if setup.GraphSHA256 != s.GraphSHA256 {
		return nil, errors.New("the realm's graph changed since the run started, so its positions no longer hold; start a new run")
	}
	if setup.BriefSHA256 != s.BriefSHA256 {
		return nil, errors.New("the brief changed since the run started; a brief is frozen, so start a new run")
	}
	if setup.Skills.Hash != s.Skills.Hash {
		return nil, errors.New("the skills the graph names changed since the run started, so results would not compare; start a new run")
	}
	if err := s.checkRealms(setup); err != nil {
		return nil, err
	}
	if setup.Root != s.Root || setup.ProjectPath != s.ProjectPath {
		return nil, fmt.Errorf("the run belongs to %s in %s, not this project", s.ProjectPath, s.Root)
	}
	if err := s.check(setup); err != nil {
		return nil, fmt.Errorf("%s/run.json: %w", dir, err)
	}
	l, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"), s.Ledger)
	if err != nil {
		return nil, err
	}
	if s.Exec != nil {
		if err := stopLeftover(s.Exec); err != nil {
			l.Close()
			return nil, err
		}
		s.Exec = nil
	}
	r := &Run{Project: setup.Project.Dir, Dir: dir, State: s, Setup: setup, Ledger: l, unlock: func() {}}
	if r.nodes, err = setup.NodesFor(s); err != nil { // State.check made it once, so this holds
		l.Close()
		return nil, err
	}
	// The spends the journal missed are written before the run goes on, so the day's cap counts them,
	// and before anything else changes, so a refusal while the journal is still broken leaves the
	// sandboxes and the leaves as they are. run.json is written as it was loaded, less what was
	// written: no switch is applied yet.
	if jerr, serr := r.appendPending(func() error { return s.Save(dir) }); jerr != nil || serr != nil {
		l.Close()
		return nil, errors.Join(r.journalError(jerr), serr)
	}
	if err := r.openSandbox(setup.newSandbox); err != nil {
		l.Close()
		return nil, err
	}
	// A ygg that was killed leaves its sandboxes up: stop them before the run goes on.
	if err := r.DownSandboxes(ctx); err != nil {
		l.Close()
		return nil, fmt.Errorf("stopping the sandboxes an earlier ygg left; ygg doctor can stop them: %w", err)
	}
	if err := r.restoreLeaves(ctx); err != nil {
		l.Close()
		return nil, err
	}
	// The switches apply in memory now, and reach the ledger and run.json with the command's first
	// write (Log or Save). A refused command writes nothing, so it switches nothing.
	if setup.ConfigSHA256 != s.ConfigSHA256 {
		s.ConfigSHA256 = setup.ConfigSHA256
		s.Roles = roles(setup)
		r.changes = append(r.changes, gungnir.Event{Name: gungnir.EventConfigChanged, ConfigHash: setup.ConfigSHA256})
	}
	if mode != s.Mode {
		r.changes = append(r.changes, gungnir.Event{Name: gungnir.EventModeChanged, Mode: mode, Looser: mode.Looser(s.Mode)})
		s.Mode = mode
	}
	if s.Status != StatusDone {
		if err := r.begin(ctx); err != nil {
			l.Close()
			return nil, err
		}
	}
	return r, nil
}

// Quarantines reports whether a run stopped at gate keeps its leaves as they are for investigation:
// a sandbox rule broken, or a secret in what the node wrote (spec §12). Only approving the stop
// puts the leaf back, and what it held is not saved: a secret must not land in a ref.
func Quarantines(g gungnir.Gate) bool {
	return g == gungnir.GatePolicyViolation || g == gungnir.GateRealSecrets
}

// DiscardWork puts leaf p back to its run's last checkpoint, as a resume does for a node that restarts
// (spec §4.3): what the work since then changed is saved to refs/ygg/<run>/discarded/<n> first, so
// Odin never discards work it didn't make. In a run with a sandbox that work was never scanned for
// secrets, so it is discarded and not saved (decision 12), and only commits already in git are kept.
// What happened is added to r.Saved.
func (r *Run) DiscardWork(ctx context.Context, p string) error {
	s := r.State
	rec := s.Leaves[p]
	l, err := r.Leaf(p)
	if err != nil {
		return err
	}
	saved, _ := l.Git(ctx, "for-each-ref", "--format=%(refname)", "refs/ygg/"+s.ID+"/discarded/")
	ref := fmt.Sprintf("refs/ygg/%s/discarded/%d", s.ID, strings.Count(saved, "\n")+min(len(saved), 1)+1)
	msg := "ygg: run " + s.ID + ", what resetting " + p + " to its checkpoint discarded"
	if r.Sandbox != nil {
		return r.discardUnscanned(ctx, l, p, ref, rec, msg)
	}
	if ok, err := l.Save(ctx, ref, rec.Checkpoint, msg); err != nil {
		return fmt.Errorf("saving %s before resetting it to its checkpoint: %w", p, err)
	} else if ok {
		r.Saved = append(r.Saved, fmt.Sprintf("%s: the changes since its checkpoint are saved as %s (git -C %s show --stat %s)", p, ref, r.RepoDir(p), ref))
	}
	if err := l.Reset(ctx, rec.Branch, rec.Checkpoint); err != nil {
		return fmt.Errorf("resetting %s to its checkpoint: %w", p, err)
	}
	return nil
}

// restoreLeaves puts every leaf back to its checkpoint when the run's node restarts (interrupted
// or failed), and otherwise refuses a leaf that changed while the run waited: Odin never discards
// work it didn't make. In a run with a sandbox, the working tree a node left is discarded unsaved,
// since no scan has seen it (decision 12). A run stopped at policy-violation is quarantined: its
// leaves stay as they are for investigation, and only approving the stop resets them (spec §12).
func (r *Run) restoreLeaves(ctx context.Context) error {
	s := r.State
	if s.Status == StatusDone || Quarantines(s.PendingGate) {
		return nil
	}
	restart := s.Status == StatusRunning || s.Status == StatusFailed || s.Status == StatusPaused
	for _, p := range slices.Sorted(maps.Keys(s.Leaves)) {
		rec := s.Leaves[p]
		l, err := r.Leaf(p)
		if err != nil {
			return err
		}
		if restart {
			if err := r.DiscardWork(ctx, p); err != nil {
				return err
			}
			continue
		}
		head, err := l.Head(ctx)
		if err != nil {
			return err
		}
		st, err := l.Git(ctx, "status", "--porcelain")
		if err != nil {
			return err
		}
		if head != rec.Checkpoint || st != "" {
			return fmt.Errorf("the leaf %s changed while the run waited: it is at %s, with %d uncommitted changes, and the run's checkpoint is %s; put it back, or start a new run",
				p, head, strings.Count(st, "\n")+min(len(st), 1), rec.Checkpoint)
		}
	}
	return nil
}

// discardUnscanned resets a leaf with a sandbox to its checkpoint without saving its working tree,
// which no scan has seen: only commits on the run's branch past the checkpoint, already in git, are
// kept at ref. The run notes what it discarded.
func (r *Run) discardUnscanned(ctx context.Context, l *leaf.Leaf, p, ref string, rec LeafRecord, msg string) error {
	st, err := l.Git(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	kept, err := l.KeepCommits(ctx, ref, rec.Checkpoint, msg)
	if err != nil {
		return fmt.Errorf("keeping the commits of %s before resetting it to its checkpoint: %w", p, err)
	}
	if err := l.Reset(ctx, rec.Branch, rec.Checkpoint); err != nil {
		return fmt.Errorf("resetting %s to its checkpoint: %w", p, err)
	}
	var notes []string
	if st != "" {
		notes = append(notes, "the uncommitted changes since its checkpoint are discarded and not saved: they ran in the sandbox and were never scanned for secrets")
	}
	if kept {
		notes = append(notes, fmt.Sprintf("the commits on its branch past the checkpoint are kept as %s (git -C %s log %s)", ref, r.RepoDir(p), ref))
	}
	if len(notes) > 0 {
		r.Saved = append(r.Saved, p+": "+strings.Join(notes, "; "))
	}
	return nil
}

// LeftoverProcess reports whether the exec node run.json recorded still has processes running.
func LeftoverProcess(p *ExecProcess) bool { return groupAlive(p.PGID) }

// StopLeftoverProcess stops the process group of an exec node that outlived its ygg, as a resume
// does, and says why when it can't.
func StopLeftoverProcess(p *ExecProcess) error { return stopLeftover(p) }

// stopLeftover stops the process group of an exec node that outlived its ygg process (killed, or
// its terminal closed), so the node never runs again beside its old copy. It kills the group only
// when the group's leader is the process run.json recorded. A group it can't tell is the run's is
// left alone, and the run waits for the operator.
func stopLeftover(p *ExecProcess) error {
	switch {
	case p.Start != 0 && procStart(p.PGID) == p.Start:
		if err := syscall.Kill(-p.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stopping node %s's leftover processes (group %d): %w", p.Node, p.PGID, err)
		}
	case groupAlive(p.PGID):
		return fmt.Errorf("node %s's process group %d may still be running from an earlier ygg; "+
			"check it with 'ps -g %d', stop it, then try again", p.Node, p.PGID, p.PGID)
	default:
		return nil // the group is gone, or only zombies are left
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !groupAlive(p.PGID) {
			return nil
		}
	}
	return fmt.Errorf("node %s's leftover process group %d didn't stop within 5s", p.Node, p.PGID)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
