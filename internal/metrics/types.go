// Package metrics reads a finished or running run's ledger, run.json and score.json, and answers the questions
// of spec §10.2: what the run cost, how long the factory worked, how often a loop passed first time, how many
// times a person had to step in, and what the hidden suite scored. It writes nothing. Every number comes from a
// ledger whose hash chain checked out, so a ledger that was changed gives no numbers at all.
package metrics

import ()

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
	Usage        Usage              `json:"usage"`
	ByRole       map[string]Usage   `json:"by_role"` // "role adapter model": what its sessions used
	Retries      int                `json:"retries"`
}
