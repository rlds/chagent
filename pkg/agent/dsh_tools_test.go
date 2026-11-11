package agent

import (
	"os"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

func newDSHTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir, err := os.MkdirTemp("", "dsh-tools-agent-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = dir
	return cfg
}

func toolNames(reg interface{ List() []string }) map[string]struct{} {
	out := make(map[string]struct{})
	for _, name := range reg.List() {
		out[name] = struct{}{}
	}
	return out
}

// TestDSHToolGroupRegisteredByDefault locks the independent DSH tool group:
// tools that are not identical to existing Chagent tools are registered by
// default, bypassing the core tool toggles but honouring the group switch.
func TestDSHToolGroupRegisteredByDefault(t *testing.T) {
	cfg := newDSHTestConfig(t)
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})
	t.Cleanup(func() { _ = agent.Close() })

	names := toolNames(agent.Tools)
	for _, want := range []string{
		"glob", "grep",
		"job_run", "job_list", "job_output", "job_kill",
		"session_search", "session_trace",
		"create_goal", "get_goal", "update_goal",
	} {
		if _, ok := names[want]; !ok {
			t.Fatalf("DSH tool %q not registered; tools=%v", want, agent.Tools.List())
		}
	}
	// Functionally identical tools must not be duplicated by the DSH group.
	for _, dup := range []string{"read_file", "write_file", "exec"} {
		if _, ok := names[dup]; !ok {
			t.Fatalf("core tool %q missing; tools=%v", dup, agent.Tools.List())
		}
	}
}

func TestDSHToolGroupCanBeDisabled(t *testing.T) {
	cfg := newDSHTestConfig(t)
	disabled := false
	cfg.Tools.DSH.Enabled = &disabled
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})
	t.Cleanup(func() { _ = agent.Close() })

	names := toolNames(agent.Tools)
	for _, unwanted := range []string{"glob", "grep", "job_run", "session_search", "create_goal"} {
		if _, ok := names[unwanted]; ok {
			t.Fatalf("DSH tool %q must be absent when tools.dsh.enabled=false; tools=%v", unwanted, agent.Tools.List())
		}
	}
}

func TestDSHToolGroupSubGroupToggles(t *testing.T) {
	cfg := newDSHTestConfig(t)
	off := false
	cfg.Tools.DSH.FSSearch.Enabled = &off
	cfg.Tools.DSH.Jobs.Enabled = &off
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})
	t.Cleanup(func() { _ = agent.Close() })

	names := toolNames(agent.Tools)
	for _, unwanted := range []string{"glob", "grep", "job_run", "job_list"} {
		if _, ok := names[unwanted]; ok {
			t.Fatalf("DSH sub-group tool %q must be absent; tools=%v", unwanted, agent.Tools.List())
		}
	}
	for _, want := range []string{"session_search", "create_goal"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("enabled DSH sub-group tool %q missing; tools=%v", want, agent.Tools.List())
		}
	}
}
