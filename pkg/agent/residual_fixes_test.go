package agent

import (
	"os"
	"testing"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
)

func TestSetLastUsageSanitizesNegativeCounters(t *testing.T) {
	ts := &turnState{}
	usage := &providers.UsageInfo{
		PromptTokens:     -5,
		CompletionTokens: -7,
		TotalTokens:      -12,
		CachedTokens:     -1,
	}
	ts.SetLastUsage(usage)
	got := ts.GetLastUsage()
	if got == nil {
		t.Fatal("GetLastUsage = nil")
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.TotalTokens != 0 || got.CachedTokens != 0 {
		t.Fatalf("negative usage not sanitized: %+v", got)
	}
	// The stored value must be a copy: mutating the provider's object later
	// must not change the recorded anchor.
	usage.PromptTokens = 123
	if ts.GetLastUsage().PromptTokens != 0 {
		t.Fatal("SetLastUsage stored the provider pointer instead of a copy")
	}
}

func TestNegativeMaxToolIterationsClampedToOne(t *testing.T) {
	dir, err := os.MkdirTemp("", "neg-max-iter-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = dir
	cfg.Agents.Defaults.MaxToolIterations = -3
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})
	t.Cleanup(func() { _ = agent.Close() })
	if agent.MaxIterations != 1 {
		t.Fatalf("MaxIterations = %d, want 1 for a negative configured value", agent.MaxIterations)
	}
}
