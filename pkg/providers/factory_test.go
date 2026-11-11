package providers

import (
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

func TestCreateProviderReturnsHTTPProviderForOpenRouter(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.ModelName = "test-openrouter"
	modelCfg := &config.ModelConfig{
		ModelName: "test-openrouter",
		Model:     "openrouter/auto",
		APIBase:   "https://openrouter.ai/api/v1",
	}
	modelCfg.SetAPIKey("sk-or-test")
	cfg.ModelList = []*config.ModelConfig{modelCfg}

	provider, _, err := CreateProvider(cfg)
	if err != nil {
		t.Fatalf("CreateProvider() error = %v", err)
	}

	if _, ok := provider.(*HTTPProvider); !ok {
		t.Fatalf("provider type = %T, want *HTTPProvider", provider)
	}
}
