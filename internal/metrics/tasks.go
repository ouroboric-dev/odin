package metrics

import (
	"slices"
	"strings"
)

// Task is one task of the plan with what it used.
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Leaf   string `json:"leaf"`
	Status string `json:"status"`
	Why    string `json:"why,omitempty"`
	Usage  Usage  `json:"usage"`

	Size     string `json:"size,omitempty"`     // the plan's estimate: S, M or L
	Attempts int    `json:"attempts,omitempty"` // the loop attempts the task took
	Lines    *Lines `json:"lines,omitempty"`    // what the task added, once it is done

	Findings []TaskFindings `json:"findings,omitempty"` // by source and severity, sorted
}

// TaskFindings is how many findings of one source and severity a task's loop raised, and how many of them closed.
type TaskFindings struct {
	Source   string `json:"source"`
	Severity string `json:"severity"`
	Raised   int    `json:"raised"`
	Closed   int    `json:"closed"`
}

// Group is the tasks of one leaf.
type Group struct {
	Tasks   int   `json:"tasks"`
	Done    int   `json:"done"`
	Blocked int   `json:"blocked"`
	Usage   Usage `json:"usage"`
}

// BlockedTask is a task the run gave up, with why.
type BlockedTask struct {
	Task string `json:"task"`
	Why  string `json:"why"`
}

// tasks lists the plan's tasks with what each used, and groups them by leaf.
func (r *Run) tasks(state *record, usage map[string]Usage) {
	for _, f := range state.Features {
		t := Task{ID: f.ID, Title: f.Title, Leaf: f.Layer, Status: string(f.Status), Why: f.Why, Usage: usage[f.Slug()]}
		r.Tasks = append(r.Tasks, t)
		g := r.Leaves[f.Layer]
		g.Tasks++
		g.Usage.add(t.Usage)
		switch f.Status {
		case "done":
			g.Done++
		case "blocked":
			g.Blocked++
			r.BlockedTasks = append(r.BlockedTasks, BlockedTask{Task: f.ID, Why: f.Why})
		}
		r.Leaves[f.Layer] = g
	}
}

// taskSlugs are the slugs of the plan's tasks, the longest first, so that a slug that extends another wins.
func taskSlugs(state *record) []string {
	var out []string
	for _, f := range state.Features {
		out = append(out, f.Slug())
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// taskOf finds the task a session's node belongs to. Session lines carry no task id; a node's id is its task's
// slug, a dash and the node's own id, and a reviewer's key shares that prefix.
func taskOf(node string, slugs []string) string {
	for _, s := range slugs {
		if strings.HasPrefix(node, s+"-") {
			return s
		}
	}
	return ""
}
