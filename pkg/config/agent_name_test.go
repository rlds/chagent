package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDerivePicoTokenStableAndNameBased(t *testing.T) {
	if ResolveAgentName("") != "default" {
		t.Fatalf("empty name should resolve to default")
	}
	a := DerivePicoToken("agent-a")
	b := DerivePicoToken("agent-a")
	c := DerivePicoToken("agent-b")
	if a != b {
		t.Fatal("derived token must be deterministic")
	}
	if a == c {
		t.Fatal("derived token must differ by name")
	}
	if len(a) != 32 {
		t.Fatalf("derived token length = %d, want 32 md5 hex chars", len(a))
	}
}

func TestNameDerivedPicoTokenAutoEnablesChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
  "version": 2,
  "model_list": [{"model_name":"m","model":"m","api_key":"k"}]
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(AgentNameEnv, "chess-agent")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pico := cfg.Channels.Get("pico")
	if pico == nil || !pico.Enabled {
		t.Fatal("pico channel should be auto-enabled even when channels is absent")
	}
	decoded, err := pico.GetDecoded()
	if err != nil {
		t.Fatal(err)
	}
	settings := decoded.(*PicoSettings)
	want := DerivePicoToken("chess-agent")
	if settings.Token.String() != want {
		t.Fatalf("token = %q, want md5(name) %q", settings.Token.String(), want)
	}
	// config.json must not contain a token: the convention is external.
	data, _ := os.ReadFile(path)
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if channels, ok := top["channels"].(map[string]any); ok {
		if picoRaw, ok := channels["pico"].(map[string]any); ok {
			if _, has := picoRaw["token"]; has {
				t.Fatal("auto pico token must not be written to config.json")
			}
		}
	}
}

func TestMCPActiveWithoutEnabledFlag(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Tools.IsToolEnabled("mcp") {
		t.Fatal("MCP should be inactive by default")
	}
	cfg.Tools.MCP.Servers = map[string]MCPServerConfig{
		"chess": {URL: "https://example.invalid/mcp", Type: "http", Enabled: true},
	}
	if !cfg.Tools.IsToolEnabled("mcp") {
		t.Fatal("configuring MCP servers should activate MCP without mcp.enabled")
	}
}
