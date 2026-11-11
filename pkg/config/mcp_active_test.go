package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMCPActiveConfig 写入一份最小 v2 配置，并附加给定的 tools JSON（可为空）。
func writeMCPActiveConfig(t *testing.T, toolsJSON string) string {
	t.Helper()

	body := `{"version":2,"model_list":[{"model_name":"m","model":"m","api_key":"k"}]`
	if strings.TrimSpace(toolsJSON) != "" {
		body += `,"tools":` + toolsJSON
	}
	body += `}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvWorkspace, "")
	t.Setenv(AgentNameEnv, "mcp-active-agent")
	return path
}

// TestLoadConfigMCPActiveSemantics locks the semantic matrix for
// tools.mcp.enabled (omitted/true/false) x (servers present/absent):
//   - omitted enabled + servers  -> MCP active (servers-only auto-enable)
//   - explicit true + servers    -> MCP active
//   - explicit false + servers   -> MCP disabled (explicit opt-out wins)
//   - omitted/true/false without servers -> only explicit true is active
//   - JSON null is treated as omitted
func TestLoadConfigMCPActiveSemantics(t *testing.T) {
	const servers = `"servers":{"cfgcap":{"enabled":true,"type":"stdio","command":"fake-mcp"}}`

	tests := []struct {
		name             string
		toolsJSON        string
		wantActive       bool
		wantExplicitDown bool
	}{
		{
			name:       "servers only, enabled omitted",
			toolsJSON:  `{"mcp":{` + servers + `}}`,
			wantActive: true,
		},
		{
			name:       "servers + explicit enabled true",
			toolsJSON:  `{"mcp":{"enabled":true,` + servers + `}}`,
			wantActive: true,
		},
		{
			name:             "servers + explicit enabled false",
			toolsJSON:        `{"mcp":{"enabled":false,` + servers + `}}`,
			wantActive:       false,
			wantExplicitDown: true,
		},
		{
			name:       "servers + enabled null treated as omitted",
			toolsJSON:  `{"mcp":{"enabled":null,` + servers + `}}`,
			wantActive: true,
		},
		{
			name:             "no servers + explicit enabled false",
			toolsJSON:        `{"mcp":{"enabled":false}}`,
			wantActive:       false,
			wantExplicitDown: true,
		},
		{
			name:       "no servers + enabled omitted",
			toolsJSON:  `{"mcp":{}}`,
			wantActive: false,
		},
		{
			name:       "no tools section at all",
			toolsJSON:  "",
			wantActive: false,
		},
		{
			name:       "no servers + explicit enabled true",
			toolsJSON:  `{"mcp":{"enabled":true}}`,
			wantActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeMCPActiveConfig(t, tt.toolsJSON)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			mcpCfg := cfg.Tools.MCP
			if got := mcpCfg.Active(); got != tt.wantActive {
				t.Fatalf("MCP.Active() = %v, want %v (ExplicitDisabled=%v Enabled=%v servers=%d)",
					got, tt.wantActive, mcpCfg.ExplicitDisabled, mcpCfg.Enabled, len(mcpCfg.Servers))
			}
			if mcpCfg.ExplicitDisabled != tt.wantExplicitDown {
				t.Fatalf("MCP.ExplicitDisabled = %v, want %v", mcpCfg.ExplicitDisabled, tt.wantExplicitDown)
			}
			// IsToolEnabled("mcp") 是 agent/commands 侧的实际入口，必须与 Active 一致。
			if got := cfg.Tools.IsToolEnabled("mcp"); got != tt.wantActive {
				t.Fatalf("IsToolEnabled(\"mcp\") = %v, want %v", got, tt.wantActive)
			}
		})
	}
}

// TestMCPExplicitDisabledIsNotSerialized guards the json:"-" contract: the
// load-time-only marker must not leak into config output or change the
// on-disk tool.mcp.enabled field.
func TestMCPExplicitDisabledIsNotSerialized(t *testing.T) {
	path := writeMCPActiveConfig(
		t,
		`{"mcp":{"enabled":false,"servers":{"cfgcap":{"enabled":true,"type":"stdio","command":"fake-mcp"}}}}`,
	)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !cfg.Tools.MCP.ExplicitDisabled {
		t.Fatal("ExplicitDisabled should be true for an explicit enabled=false")
	}

	encoded, err := json.Marshal(cfg.Tools.MCP)
	if err != nil {
		t.Fatalf("json.Marshal(MCPConfig): %v", err)
	}
	if strings.Contains(string(encoded), "ExplicitDisabled") ||
		strings.Contains(string(encoded), "explicit_disabled") {
		t.Fatalf("ExplicitDisabled leaked into serialized MCP config: %s", encoded)
	}

	var roundTrip map[string]any
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("unmarshal serialized MCP config: %v", err)
	}
	if roundTrip["enabled"] != false {
		t.Fatalf("serialized enabled = %v, want false", roundTrip["enabled"])
	}

	// Reloading the marshalled form must not re-enable MCP: the explicit false is
	// still on disk even though the internal marker is gone.
	dir := t.TempDir()
	reloadPath := filepath.Join(dir, "config.json")
	reloadBody := `{"version":2,"model_list":[{"model_name":"m","model":"m","api_key":"k"}],"tools":{"mcp":` +
		string(encoded) + `}}`
	if err := os.WriteFile(reloadPath, []byte(reloadBody), 0o600); err != nil {
		t.Fatalf("write reload config: %v", err)
	}
	reloaded, err := LoadConfig(reloadPath)
	if err != nil {
		t.Fatalf("LoadConfig(reload) error = %v", err)
	}
	if reloaded.Tools.MCP.Active() {
		t.Fatal("explicit enabled=false must survive marshal/reload and keep MCP disabled")
	}
}
