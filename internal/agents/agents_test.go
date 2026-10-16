package agents

import (
	"slices"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
)

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
