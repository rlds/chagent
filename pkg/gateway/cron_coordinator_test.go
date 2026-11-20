package gateway

import (
	"encoding/json"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

func TestNewCronCoordinatorFromPicoConfig(t *testing.T) {
	t.Setenv(config.AgentNameEnv, "daily-rounds")
	t.Setenv("CHAGENT_CRON_COORDINATOR_URL", "http://127.0.0.1:18079")
	cfg := config.DefaultConfig()
	cfg.Channels[config.ChannelPico] = &config.Channel{
		Settings: config.RawNode(json.RawMessage(`{"token":"pico-token"}`)),
	}
	c := newCronCoordinator(cfg)
	if c == nil {
		t.Fatal("coordinator should be configured")
	}
	if c.Agent != "daily-rounds" || c.Token != "pico-token" || c.BaseURL != "http://127.0.0.1:18079" {
		t.Fatalf("coordinator = %+v", c)
	}
}

func TestNewCronCoordinatorDisabledWithoutToken(t *testing.T) {
	t.Setenv(config.AgentNameEnv, "daily-rounds")
	cfg := config.DefaultConfig()
	cfg.Channels[config.ChannelPico] = &config.Channel{Settings: config.RawNode(json.RawMessage(`{}`))}
	if c := newCronCoordinator(cfg); c != nil {
		t.Fatalf("coordinator should be nil without token: %+v", c)
	}
}
