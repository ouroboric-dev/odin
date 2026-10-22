// Package metrics reads a finished or running run's ledger, run.json and score.json, and answers the questions
// of spec §10.2: what the run cost, how long the factory worked, how often a loop passed first time, how many
// times a person had to step in, and what the hidden suite scored. It writes nothing. Every number comes from a
// ledger whose hash chain checked out, so a ledger that was changed gives no numbers at all.
package metrics

import (
	"strconv"
	"time"
)

// Duration is a time.Duration that is written in JSON as whole milliseconds, which the field names promise.
type Duration time.Duration

// MarshalJSON writes d as milliseconds.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Duration(d).Milliseconds(), 10)), nil
}

// Round is d to the second, for a table.
func (d Duration) Round() time.Duration { return time.Duration(d).Round(time.Second) }

// Usage is what sessions used. Cost is the sum of the sessions' own cost fields.
type Usage struct {
	InTok        int64   `json:"in_tokens"`
	OutTok       int64   `json:"out_tokens"`
	CacheTok     int64   `json:"cache_read_tokens"`
	Turns        int64   `json:"turns"`
	CostUSD      float64 `json:"cost_usd"`
	EstimatedUSD float64 `json:"estimated_usd"` // of CostUSD: sessions whose adapter estimated the cost
	BilledUSD    float64 `json:"billed_usd"`    // of CostUSD: what a paid API billed. The rest is what subscription sessions would cost on the API
	Sessions     int     `json:"sessions"`
	Unpriced     int     `json:"unpriced_sessions"` // sessions with tokens and no cost: their cost is unknown, not zero (B-107)
}

func (u *Usage) add(o Usage) {
	u.InTok += o.InTok
	u.OutTok += o.OutTok
	u.CacheTok += o.CacheTok
	u.Turns += o.Turns
	u.CostUSD += o.CostUSD
	u.EstimatedUSD += o.EstimatedUSD
	u.BilledUSD += o.BilledUSD
	u.Sessions += o.Sessions
	u.Unpriced += o.Unpriced
}

// Time is the run's clock. Elapsed runs from the first line to the run's end, or to the last line of a run that
// has not ended. Active is what is left when the approval waits and the time the run was stopped are taken out.
type Time struct {
	Elapsed      Duration `json:"elapsed_ms"`
	Active       Duration `json:"active_ms"`
	ApprovalWait Duration `json:"approval_wait_ms"` // from gate_pending to the decision
	Stopped      Duration `json:"stopped_ms"`       // from a failure, a pause or a killed process to the resume
}

// Setting is the agent and model of a role.
type Setting struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// Run is everything ygg metrics says about one run.
type Run struct {
	ID           string             `json:"run"`
	Status       string             `json:"status"`
	Mode         string             `json:"mode"`
	Graph        string             `json:"graph"`
	GraphVersion string             `json:"graph_version"`
	Factory      []string           `json:"factory"` // the builds of ygg that wrote lines of the ledger, in the order they first did
	ConfigSHA256 string             `json:"config_sha256"`
	BriefSHA256  string             `json:"brief_sha256,omitempty"`
	Roles        map[string]Setting `json:"roles"`
	Realms       map[string]string  `json:"realms"` // realm name: version
	Time         Time               `json:"time"`
	Usage        Usage              `json:"usage"`
	ByRole       map[string]Usage   `json:"by_role"` // "role adapter model": what its sessions used
	Tasks        []Task             `json:"tasks"`
	Leaves       map[string]Group   `json:"leaves"`
	BlockedTasks []BlockedTask      `json:"blocked_tasks"`
	Retries      int                `json:"retries"`
	Warnings     []string           `json:"warnings"` // things that do not add up, such as a ledger and run.json that disagree

	Loops         Loops         `json:"loops"`
	Review        Review        `json:"review"`
	Findings      Findings      `json:"findings"`
	Interventions Interventions `json:"interventions"`
}
