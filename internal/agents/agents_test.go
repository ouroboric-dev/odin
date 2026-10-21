package agents

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/ouroboric-dev/gleipnir/opencode"
	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// effortOf builds the adapter of role and reads the reasoning effort that its session config names for the model.
// ok is false when the config names none.
func effortOf(t *testing.T, role schema.Role) (effort string, ok bool) {
	t.Helper()
	a, err := New(role, t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	oc, isOpenCode := a.(*opencode.Agent)
	if !isOpenCode {
		t.Fatalf("New(%s) is %T, want the OpenCode adapter", role.Agent, a)
	}
	conf, err := oc.ConfigJSON(gungnir.Task{})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Provider map[string]struct {
			Models map[string]struct {
				Options struct {
					Reasoning map[string]string `json:"reasoning"`
				} `json:"options"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(conf), &c); err != nil {
		t.Fatal(err)
	}
	r := c.Provider["openrouter"].Models[role.Model[len("openrouter/"):]].Options.Reasoning
	effort, ok = r["effort"]
	return effort, ok
}

func TestARoleReasoningReachesTheOpenCodeConfig(t *testing.T) {
	for _, v := range []string{"low", "medium", "high"} {
		if got, ok := effortOf(t, schema.Role{Agent: "opencode", Model: "openrouter/x/y", Reasoning: v}); !ok || got != v {
			t.Errorf("reasoning: %s gave effort %q (named: %v)", v, got, ok)
		}
	}
	if got, ok := effortOf(t, schema.Role{Agent: "opencode", Model: "openrouter/x/y", Reasoning: "off"}); ok {
		t.Errorf("reasoning: off named effort %q, want none", got)
	}
}

func TestARoleWithoutReasoningGetsHighEffort(t *testing.T) {
	if got, ok := effortOf(t, schema.Role{Agent: "opencode", Model: "openrouter/x/y"}); !ok || got != "high" {
		t.Errorf("an unset reasoning gave effort %q (named: %v), want high", got, ok)
	}
}

func TestOtherAdaptersIgnoreReasoning(t *testing.T) {
	a, err := New(schema.Role{Agent: "claude-code", Model: "sonnet", Reasoning: "low"}, t.TempDir(), Options{})
	if err != nil || a.Name() != "claude-code" {
		t.Errorf("a claude-code role with reasoning: %v, %v", a, err)
	}
	b, err := New(schema.Role{Agent: "claude-code", Model: "sonnet"}, t.TempDir(), Options{})
	if err != nil || b.Name() != a.Name() || !reflect.DeepEqual(a, b) {
		t.Errorf("the setting changed the adapter: %v vs %v (%v)", a, b, err)
	}
}

func TestNewBuildsEachAdapterAndRefusesWhatItCannotTighten(t *testing.T) {
	for agent, model := range map[string]string{"claude-code": "sonnet", "opencode": "openrouter/deepseek/deepseek-v4-flash"} {
		a, err := New(schema.Role{Agent: agent, Model: model}, t.TempDir(), Options{Privacy: "strict"})
		if err != nil || a.Name() != agent {
			t.Errorf("New(%s) = %v, %v", agent, a, err)
		}
	}
	if _, err := New(schema.Role{Agent: "opencode", Model: "anthropic/claude"}, t.TempDir(), Options{Privacy: "strict"}); err == nil {
		t.Error("privacy: strict on a model the adapter can't tighten was accepted")
	}
	if _, err := New(schema.Role{Agent: "codex", Model: "x"}, t.TempDir(), Options{}); err == nil {
		t.Error("an unknown agent was accepted")
	}
	a, err := New(schema.Role{Agent: "opencode", Model: "openrouter/x/y", Price: &schema.Price{Input: 1, Output: 2}}, t.TempDir(), Options{})
	if err != nil || a == nil {
		t.Errorf("a priced role: %v", err)
	}
}

func TestHostsAreWhatTheModelAPIsNeed(t *testing.T) {
	if !slices.Equal(Hosts("claude-code"), []string{"api.anthropic.com"}) || !slices.Equal(Hosts("opencode"), []string{"openrouter.ai"}) || Hosts("scripted") != nil {
		t.Errorf("hosts = %v %v %v", Hosts("claude-code"), Hosts("opencode"), Hosts("scripted"))
	}
}
