package agents

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/opencode"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

func localList() *schema.ModelList {
	return &schema.ModelList{
		Server: schema.ModelServerConfig{Name: "llama", Network: "ygg-models", Port: 8080},
		Models: map[string]schema.LocalModelConfig{
			"qwen": {File: "q.gguf", Context: 32768, Output: 4096, Price: schema.Price{Input: 0.1, Output: 0.2}},
		},
	}
}

// fakeRunner stands in for the sandbox: it answers the version probe and prints one step of a million tokens
// in and a million out, with a cost of zero, as a local model's session does.
type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	if c.Argv[1] == "--version" {
		c.Stdout.Write([]byte("1.18.32\n"))
		return gungnir.ExecResult{}, nil
	}
	c.Stdout.Write([]byte(`{"type":"text","sessionID":"s","part":{"type":"text","text":"ok"}}` + "\n" +
		`{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop","tokens":{"total":2000000,"input":1000000,"output":1000000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}` + "\n"))
	return gungnir.ExecResult{}, nil
}

// session builds the role's adapter and runs one fake session, returning the adapter's config and the session's cost.
func session(t *testing.T, role schema.Role, o Options) (conf string, cost float64) {
	t.Helper()
	a, err := New(role, t.TempDir(), o)
	if err != nil {
		t.Fatal(err)
	}
	tk := gungnir.Task{Node: "n", Attempt: 1, Prompt: "p", Model: role.Model, Sandbox: fakeRunner{}, TranscriptDir: t.TempDir()}
	conf, err = a.(*opencode.Agent).ConfigJSON(tk)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	return conf, res.Usage.CostUSD
}

func TestNewLocalRoleTakesTheListPrice(t *testing.T) {
	conf, cost := session(t, schema.Role{Agent: "opencode", Model: "local/qwen"}, Options{Models: localList()})
	for _, want := range []string{"http://llama:8080/v1", "32768", "4096"} {
		if !strings.Contains(conf, want) {
			t.Errorf("the config lacks %q:\n%s", want, conf)
		}
	}
	if math.Abs(cost-0.3) > 1e-9 {
		t.Errorf("cost = %v, want the list's price: 0.1 + 0.2", cost)
	}
	_, cost = session(t, schema.Role{Agent: "opencode", Model: "local/qwen", Price: &schema.Price{Input: 3, Output: 4}}, Options{Models: localList()})
	if math.Abs(cost-7) > 1e-9 {
		t.Errorf("cost = %v, want the role's price: 3 + 4", cost)
	}
	if _, err := New(schema.Role{Agent: "opencode", Model: "local/qwen"}, t.TempDir(), Options{}); err == nil {
		t.Error("a local role without a model list was accepted")
	}
	if _, err := New(schema.Role{Agent: "opencode", Model: "local/other"}, t.TempDir(), Options{Models: localList()}); err == nil {
		t.Error("a local role whose model is not in the list was accepted")
	}
}

func TestLocalHostsAreNone(t *testing.T) {
	if h := Hosts("opencode", "local/qwen"); h != nil {
		t.Errorf("a local model's hosts = %v, want none", h)
	}
	if h := Hosts("opencode", "openrouter/x/y"); len(h) != 1 || h[0] != "openrouter.ai" {
		t.Errorf("an OpenRouter model's hosts = %v", h)
	}
}
