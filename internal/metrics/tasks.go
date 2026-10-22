package metrics

// Task is one task of the plan with what it used.
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Leaf   string `json:"leaf"`
	Status string `json:"status"`
	Why    string `json:"why,omitempty"`
	Usage  Usage  `json:"usage"`
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
