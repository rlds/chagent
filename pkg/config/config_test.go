package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rlds/chagent/pkg"
	"github.com/rlds/chagent/pkg/credential"
)

// mustSetupSSHKey generates a temporary Ed25519 SSH key in t.TempDir() and sets
// CHAGENT_SSH_KEY_PATH to its path for the duration of the test. This is required
// whenever a test exercises encryption/decryption via credential.Encrypt or SaveConfig.
func mustSetupSSHKey(t *testing.T) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "chagent_ed25519.key")
	if err := credential.GenerateSSHKey(keyPath); err != nil {
		t.Fatalf("mustSetupSSHKey: %v", err)
	}
	t.Setenv("CHAGENT_SSH_KEY_PATH", keyPath)
}

func TestAgentModelConfig_UnmarshalString(t *testing.T) {
	var m AgentModelConfig
	if err := json.Unmarshal([]byte(`"gpt-4"`), &m); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if m.Primary != "gpt-4" {
		t.Errorf("Primary = %q, want 'gpt-4'", m.Primary)
	}
	if m.Fallbacks != nil {
		t.Errorf("Fallbacks = %v, want nil", m.Fallbacks)
	}
}

func TestAgentModelConfig_UnmarshalObject(t *testing.T) {
	var m AgentModelConfig
	data := `{"primary": "claude-opus", "fallbacks": ["gpt-4o-mini", "haiku"]}`
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if m.Primary != "claude-opus" {
		t.Errorf("Primary = %q, want 'claude-opus'", m.Primary)
	}
	if len(m.Fallbacks) != 2 {
		t.Fatalf("Fallbacks len = %d, want 2", len(m.Fallbacks))
	}
	if m.Fallbacks[0] != "gpt-4o-mini" || m.Fallbacks[1] != "haiku" {
		t.Errorf("Fallbacks = %v", m.Fallbacks)
	}
}

func TestAgentModelConfig_MarshalString(t *testing.T) {
	m := AgentModelConfig{Primary: "gpt-4"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `"gpt-4"` {
		t.Errorf("marshal = %s, want '\"gpt-4\"'", string(data))
	}
}

func TestAgentModelConfig_MarshalObject(t *testing.T) {
	m := AgentModelConfig{Primary: "claude-opus", Fallbacks: []string{"haiku"}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	json.Unmarshal(data, &result)
	if result["primary"] != "claude-opus" {
		t.Errorf("primary = %v", result["primary"])
	}
}

func TestAgentConfig_FullParse(t *testing.T) {
	jsonData := `{
		"agents": {
			"defaults": {
				"workspace": "~/.chagent/workspace",
				"model": "glm-4.7",
				"max_tokens": 8192,
				"max_tool_iterations": 20
			},
			"list": [
				{
					"id": "sales",
					"default": true,
					"name": "Sales Bot",
					"model": "gpt-4"
				},
			{
				"id": "support",
				"name": "Support Bot",
				"model": {
					"primary": "claude-opus",
					"fallbacks": ["haiku"]
				},
				"subagents": {
					"allow_agents": ["sales"]
				}
			}
			]
		},
		"session": {
			"dimensions": ["sender"],
			"identity_links": {
				"john": ["telegram:123", "discord:john#1234"]
			}
		}
	}`

	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(jsonData), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(cfg.Agents.List) != 2 {
		t.Fatalf("agents.list len = %d, want 2", len(cfg.Agents.List))
	}

	sales := cfg.Agents.List[0]
	if sales.ID != "sales" || !sales.Default || sales.Name != "Sales Bot" {
		t.Errorf("sales = %+v", sales)
	}
	if sales.Model == nil || sales.Model.Primary != "gpt-4" {
		t.Errorf("sales.Model = %+v", sales.Model)
	}

	support := cfg.Agents.List[1]
	if support.ID != "support" || support.Name != "Support Bot" {
		t.Errorf("support = %+v", support)
	}
	if support.Model == nil || support.Model.Primary != "claude-opus" {
		t.Errorf("support.Model = %+v", support.Model)
	}
	if len(support.Model.Fallbacks) != 1 || support.Model.Fallbacks[0] != "haiku" {
		t.Errorf("support.Model.Fallbacks = %v", support.Model.Fallbacks)
	}
	if support.Subagents == nil || len(support.Subagents.AllowAgents) != 1 {
		t.Errorf("support.Subagents = %+v", support.Subagents)
	}

	if len(cfg.Session.Dimensions) != 1 || cfg.Session.Dimensions[0] != "sender" {
		t.Errorf("Session.Dimensions = %v", cfg.Session.Dimensions)
	}
	if len(cfg.Session.IdentityLinks) != 1 {
		t.Errorf("Session.IdentityLinks = %v", cfg.Session.IdentityLinks)
	}
	links := cfg.Session.IdentityLinks["john"]
	if len(links) != 2 {
		t.Errorf("john links = %v", links)
	}
}

func TestTurnProfileConfig_ParseAndResolve(t *testing.T) {
	jsonData := `{
		"agents": {
			"defaults": {
				"turn_profile": {
					"enabled": true,
					"history": {"mode": "off"},
					"system_prompt": {"mode": "off"},
					"skills": {"mode": "off"},
					"tools": {
						"mode": "custom",
						"allow": ["web_search", "web_fetch"]
					}
				}
			}
		}
	}`

	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(jsonData), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.ValidateTurnProfile(); err != nil {
		t.Fatalf("ValidateTurnProfile() error = %v", err)
	}

	profile, ok, err := cfg.Agents.Defaults.ResolveTurnProfile()
	if err != nil {
		t.Fatalf("ResolveTurnProfile() error = %v", err)
	}
	if !ok {
		t.Fatal("ResolveTurnProfile() ok = false, want true")
	}
	if profile.HistoryMode != TurnProfileModeOff ||
		profile.SystemPromptMode != TurnProfileModeOff ||
		profile.SkillsMode != TurnProfileModeOff ||
		profile.ToolsMode != TurnProfileModeCustom {
		t.Fatalf("resolved clean_web modes = %+v", profile)
	}
	assert.Equal(t, []string{"web_search", "web_fetch"}, profile.AllowedTools)
}

func TestTurnProfileConfig_DisabledOrMissingIsNoop(t *testing.T) {
	cfg := DefaultConfig()

	profile, ok, err := cfg.Agents.Defaults.ResolveTurnProfile()
	if err != nil {
		t.Fatalf("ResolveTurnProfile(missing) error = %v", err)
	}
	if ok {
		t.Fatal("ResolveTurnProfile(missing) ok = true, want false")
	}
	if profile.Enabled {
		t.Fatalf("ResolveTurnProfile(missing) profile.Enabled = true, want false")
	}

	cfg.Agents.Defaults.TurnProfile = TurnProfileConfig{
		Enabled: false,
		History: TurnProfileBlock{
			Mode: TurnProfileModeOff,
		},
	}
	profile, ok, err = cfg.Agents.Defaults.ResolveTurnProfile()
	if err != nil {
		t.Fatalf("ResolveTurnProfile(disabled) error = %v", err)
	}
	if ok || profile.Enabled {
		t.Fatalf("disabled profile = (%+v, %v), want no-op", profile, ok)
	}

	cfg.Agents.Defaults.TurnProfile = TurnProfileConfig{
		Enabled: false,
		History: TurnProfileBlock{
			Mode: TurnProfileModeCustom,
		},
		Tools: TurnProfileBlock{
			Mode: TurnProfileMode("sometimes"),
		},
	}
	if err := cfg.ValidateTurnProfile(); err != nil {
		t.Fatalf("ValidateTurnProfile(disabled unsupported modes) error = %v, want nil", err)
	}
}

func TestTurnProfileConfig_ValidationRejectsUnsupportedModes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "history custom unsupported",
			raw:  `{"agents":{"defaults":{"turn_profile":{"enabled":true,"history":{"mode":"custom"}}}}}`,
			want: "history.mode",
		},
		{
			name: "system prompt custom unsupported",
			raw:  `{"agents":{"defaults":{"turn_profile":{"enabled":true,"system_prompt":{"mode":"custom"}}}}}`,
			want: "system_prompt.mode",
		},
		{
			name: "unknown mode",
			raw:  `{"agents":{"defaults":{"turn_profile":{"enabled":true,"tools":{"mode":"sometimes"}}}}}`,
			want: "unsupported mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := json.Unmarshal([]byte(tt.raw), cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := cfg.ValidateTurnProfile()
			if err == nil {
				t.Fatal("ValidateTurnProfile() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateTurnProfile() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDefaultConfig_MCPMaxInlineTextChars(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Tools.MCP.GetMaxInlineTextChars() != DefaultMCPMaxInlineTextChars {
		t.Fatalf(
			"DefaultConfig().Tools.MCP.GetMaxInlineTextChars() = %d, want %d",
			cfg.Tools.MCP.GetMaxInlineTextChars(),
			DefaultMCPMaxInlineTextChars,
		)
	}
}

func TestConfig_BackwardCompat_NoAgentsList(t *testing.T) {
	jsonData := `{
		"agents": {
			"defaults": {
				"workspace": "~/.chagent/workspace",
				"model": "glm-4.7",
				"max_tokens": 8192,
				"max_tool_iterations": 20
			}
		}
	}`

	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(jsonData), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(cfg.Agents.List) != 0 {
		t.Errorf("agents.list should be empty for backward compat, got %d", len(cfg.Agents.List))
	}
}

func TestAgentConfig_ParsesDispatchRules(t *testing.T) {
	jsonData := `{
		"agents": {
			"defaults": {
				"workspace": "~/.chagent/workspace",
				"model": "glm-4.7"
			},
			"list": [
				{ "id": "main", "default": true },
				{ "id": "support" }
			],
			"dispatch": {
				"rules": [
					{
						"name": "support-vip",
						"agent": "support",
						"when": {
							"channel": "telegram",
							"chat": "group:-100123",
							"sender": "12345",
							"mentioned": true
						},
						"session_dimensions": ["chat", "sender"]
					}
				]
			}
		}
	}`

	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(jsonData), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Agents.Dispatch == nil {
		t.Fatal("Agents.Dispatch should not be nil")
	}
	if len(cfg.Agents.Dispatch.Rules) != 1 {
		t.Fatalf("Dispatch.Rules len = %d, want 1", len(cfg.Agents.Dispatch.Rules))
	}
	rule := cfg.Agents.Dispatch.Rules[0]
	if rule.Name != "support-vip" || rule.Agent != "support" {
		t.Fatalf("rule = %+v", rule)
	}
	if rule.When.Channel != "telegram" || rule.When.Chat != "group:-100123" || rule.When.Sender != "12345" {
		t.Fatalf("rule.When = %+v", rule.When)
	}
	if rule.When.Mentioned == nil || !*rule.When.Mentioned {
		t.Fatalf("rule.When.Mentioned = %+v, want true", rule.When.Mentioned)
	}
	if got := rule.SessionDimensions; len(got) != 2 || got[0] != "chat" || got[1] != "sender" {
		t.Fatalf("rule.SessionDimensions = %v, want [chat sender]", got)
	}
}

// TestDefaultConfig_HeartbeatEnabled verifies heartbeat is enabled by default
func TestDefaultConfig_HeartbeatEnabled(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.Heartbeat.Enabled {
		t.Error("Heartbeat should be enabled by default")
	}
}

// TestDefaultConfig_WorkspacePath verifies workspace path is correctly set
func TestDefaultConfig_WorkspacePath(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.Workspace == "" {
		t.Error("Workspace should not be empty")
	}
}

// TestDefaultConfig_AnthropicModelsUseClaudeAPIIDs verifies that first-party
// Anthropic defaults use Claude API model IDs, not dotted display names or
// Bedrock-style provider prefixes. See:
// https://platform.claude.com/docs/en/about-claude/models/model-ids-and-versions
func TestDefaultConfig_AnthropicModelsUseClaudeAPIIDs(t *testing.T) {
	cfg := DefaultConfig()

	checked := 0
	for _, model := range cfg.ModelList {
		if model.Provider != "anthropic" {
			continue
		}
		checked++
		if strings.Contains(model.Model, ".") {
			t.Fatalf("Anthropic default model %q uses dotted ID %q", model.ModelName, model.Model)
		}
	}

	if checked == 0 {
		t.Fatal("DefaultConfig() missing Anthropic models")
	}
}

// TestDefaultConfig_MaxTokens verifies max tokens has default value
func TestDefaultConfig_MaxTokens(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.MaxTokens == 0 {
		t.Error("MaxTokens should not be zero")
	}
}

// TestDefaultConfig_MaxToolIterations verifies max tool iterations has default value
func TestDefaultConfig_MaxToolIterations(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		t.Error("MaxToolIterations should not be zero")
	}
}

// TestDefaultConfig_Temperature verifies temperature has default value
func TestDefaultConfig_Temperature(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.Temperature != nil {
		t.Error("Temperature should be nil when not provided")
	}
}

// TestDefaultConfig_Gateway verifies gateway defaults
func TestDefaultConfig_Gateway(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Gateway.Host != "127.0.0.1" {
		t.Errorf("Gateway.Host = %q, want 127.0.0.1", cfg.Gateway.Host)
	}
	if cfg.Gateway.Port != 0 {
		t.Errorf("Gateway.Port = %d, want 0 (OS auto-select)", cfg.Gateway.Port)
	}
	if cfg.Gateway.HotReload {
		t.Error("Gateway hot reload should be disabled by default")
	}
}

// TestDefaultConfig_Channels verifies channels are disabled by default
func TestDefaultConfig_Channels(t *testing.T) {
	cfg := DefaultConfig()

	for name, bc := range cfg.Channels {
		if bc.Enabled {
			t.Errorf("Channel %q should be disabled by default", name)
		}
	}
}

func TestDefaultConfig_ChannelStreamingDisabled(t *testing.T) {
	cfg := DefaultConfig()

	pico := cfg.Channels.Get(ChannelPico)
	if pico == nil {
		t.Fatal("DefaultConfig() missing pico channel")
	}
	decoded, err := pico.GetDecoded()
	if err != nil {
		t.Fatalf("pico GetDecoded() error = %v", err)
	}
	picoSettings, ok := decoded.(*PicoSettings)
	if !ok {
		t.Fatalf("pico settings type = %T, want *PicoSettings", decoded)
	}
	if !picoSettings.Streaming.Enabled {
		t.Fatal("DefaultConfig().pico.settings.streaming.enabled should be true")
	}
}

func TestValidateSingletonChannels_RejectsMultipleInstances(t *testing.T) {
	channels := ChannelsConfig{
		"pico1": &Channel{Enabled: true, Type: ChannelPico},
		"pico2": &Channel{Enabled: true, Type: ChannelPico},
	}
	err := validateSingletonChannels(channels)
	if err == nil {
		t.Fatal("expected error for multiple pico channels, got nil")
	}
	if !strings.Contains(err.Error(), "singleton") {
		t.Fatalf("expected singleton error, got: %v", err)
	}
}

func TestValidateSingletonChannels_AllowsSingleInstance(t *testing.T) {
	channels := ChannelsConfig{
		"pico1": &Channel{Enabled: true, Type: ChannelPico},
	}
	err := validateSingletonChannels(channels)
	if err != nil {
		t.Fatalf("expected no error for single pico channel, got: %v", err)
	}
}

func TestValidateSingletonChannels_IgnoresDisabledInstances(t *testing.T) {
	channels := ChannelsConfig{
		"pico1": &Channel{Enabled: true, Type: ChannelPico},
		"pico2": &Channel{Enabled: false, Type: ChannelPico},
	}
	err := validateSingletonChannels(channels)
	if err != nil {
		t.Fatalf("expected no error when only one pico channel is enabled, got: %v", err)
	}
}

// TestDefaultConfig_WebTools verifies web tools config
func TestDefaultConfig_WebTools(t *testing.T) {
	cfg := DefaultConfig()

	// Verify web tools defaults
	if cfg.Tools.Web.Brave.MaxResults != 5 {
		t.Error("Expected Brave MaxResults 5, got ", cfg.Tools.Web.Brave.MaxResults)
	}
	if len(cfg.Tools.Web.Brave.APIKeys) != 0 {
		t.Error("Brave API key should be empty by default")
	}
	if cfg.Tools.Web.DuckDuckGo.MaxResults != 5 {
		t.Error("Expected DuckDuckGo MaxResults 5, got ", cfg.Tools.Web.DuckDuckGo.MaxResults)
	}
}

func TestSaveConfig_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("config file has permission %04o, want 0600", perm)
	}
}

func TestSaveConfig_IncludesEmptyLegacyModelField(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if !strings.Contains(string(data), `"model_name": ""`) {
		t.Fatalf("saved config should include empty legacy model_name field, got: %s", string(data))
	}
}

func TestSaveConfig_PreservesDisabledTelegramPlaceholder(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	if bc := cfg.Channels.Get("telegram"); bc != nil {
		bc.Placeholder.Enabled = false
	}

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.Contains(string(data), `"placeholder": {`) {
		t.Fatalf("saved config should include telegram placeholder config, got: %s", string(data))
	}
	if !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("saved config should persist placeholder.enabled=false, got: %s", string(data))
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	bc := loaded.Channels.Get("telegram")
	if bc != nil && bc.Placeholder.Enabled {
		t.Fatal("telegram placeholder should remain disabled after SaveConfig/LoadConfig round-trip")
	}
}

func TestSaveConfig_PreservesExplicitDisabledPicoStreaming(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	pico := cfg.Channels.Get(ChannelPico)
	if pico == nil {
		t.Fatal("DefaultConfig() missing pico channel")
	}
	pico.Settings = RawNode(`{"streaming":{"enabled":false}}`)

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.Contains(string(data), `"streaming"`) || !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("saved config should preserve explicit disabled pico streaming, got:\n%s", string(data))
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	loadedPico := loaded.Channels.Get(ChannelPico)
	if loadedPico == nil {
		t.Fatal("loaded config missing pico channel")
	}
	decoded, err := loadedPico.GetDecoded()
	if err != nil {
		t.Fatalf("pico GetDecoded() error = %v", err)
	}
	settings, ok := decoded.(*PicoSettings)
	if !ok {
		t.Fatalf("pico settings type = %T, want *PicoSettings", decoded)
	}
	if settings.Streaming.Enabled {
		t.Fatal("explicit disabled pico streaming should remain disabled after SaveConfig/LoadConfig round-trip")
	}
}

// TestSaveConfig_FiltersVirtualModels verifies that SaveConfig does not write
// virtual models (generated by expandMultiKeyModels) to the config file.
func TestSaveConfig_FiltersVirtualModels(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()

	// Manually add a virtual model to ModelList (simulating what expandMultiKeyModels does)
	primaryModel := &ModelConfig{
		ModelName: "gpt-4",
		Model:     "openai/gpt-4o",
		APIKeys:   SimpleSecureStrings("key1"),
	}
	virtualModel := &ModelConfig{
		ModelName: "gpt-4__key_1",
		Model:     "openai/gpt-4o",
		APIKeys:   SimpleSecureStrings("key2"),
		isVirtual: true,
	}
	cfg.ModelList = []*ModelConfig{primaryModel, virtualModel}

	// SaveConfig should filter out virtual models
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Reload and verify
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Should only have the primary model, not the virtual one
	if len(reloaded.ModelList) != 1 {
		t.Fatalf("expected 1 model after reload, got %d", len(reloaded.ModelList))
	}

	if reloaded.ModelList[0].ModelName != "gpt-4" {
		t.Errorf("expected model_name 'gpt-4', got %q", reloaded.ModelList[0].ModelName)
	}

	// Verify virtual model was not persisted
	for _, m := range reloaded.ModelList {
		if m.ModelName == "gpt-4__key_1" {
			t.Errorf("virtual model gpt-4__key_1 should not have been saved")
		}
	}

	// Verify the saved file does not contain the virtual model name
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if strings.Contains(string(data), "gpt-4__key_1") {
		t.Errorf("saved config should not contain virtual model name 'gpt-4__key_1'")
	}
}

// TestConfig_Complete verifies all config fields are set
func TestConfig_Complete(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.Workspace == "" {
		t.Error("Workspace should not be empty")
	}
	if cfg.Agents.Defaults.Temperature != nil {
		t.Error("Temperature should be nil when not provided")
	}
	if cfg.Agents.Defaults.MaxTokens == 0 {
		t.Error("MaxTokens should not be zero")
	}
	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		t.Error("MaxToolIterations should not be zero")
	}
	if cfg.Gateway.Host != "127.0.0.1" {
		t.Errorf("Gateway.Host = %q, want 127.0.0.1", cfg.Gateway.Host)
	}
	if cfg.Gateway.Port != 0 {
		t.Errorf("Gateway.Port = %d, want 0", cfg.Gateway.Port)
	}
	if !cfg.Heartbeat.Enabled {
		t.Error("Heartbeat should be enabled by default")
	}
	if !cfg.Tools.Exec.AllowRemote {
		t.Error("Exec.AllowRemote should be true by default")
	}
}

func TestDefaultConfig_WebPreferNativeEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.Web.PreferNative {
		t.Fatal("DefaultConfig().Tools.Web.PreferNative should be true")
	}
}

func TestDefaultConfig_WebProviderIsAuto(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Tools.Web.Provider != "auto" {
		t.Fatalf("DefaultConfig().Tools.Web.Provider = %q, want auto", cfg.Tools.Web.Provider)
	}
}

func TestConfigExample_WebProviderIsAuto(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.example.json) error: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal(config.example.json) error: %v", err)
	}
	if cfg.Tools.Web.Provider != "auto" {
		t.Fatalf("config.example.json tools.web.provider = %q, want auto", cfg.Tools.Web.Provider)
	}
}

func TestDefaultConfig_ToolFeedbackDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agents.Defaults.ToolFeedback.Enabled {
		t.Fatal("DefaultConfig().Agents.Defaults.ToolFeedback.Enabled should be false")
	}
	if cfg.Agents.Defaults.ToolFeedback.SeparateMessages {
		t.Fatal("DefaultConfig().Agents.Defaults.ToolFeedback.SeparateMessages should be false")
	}
}

func TestLoadConfig_ToolFeedbackDefaultsFalseWhenUnset(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(
		configPath,
		[]byte(`{"version":2,"agents":{"defaults":{"workspace":"./workspace"}}}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if cfg.Agents.Defaults.ToolFeedback.Enabled {
		t.Fatal(
			"agents.defaults.tool_feedback.enabled should remain false when unset in config file",
		)
	}
	if cfg.Agents.Defaults.ToolFeedback.SeparateMessages {
		t.Fatal("agents.defaults.tool_feedback.separate_messages should remain false when unset in config file")
	}
}

func TestLoadConfig_WebPreferNativeDefaultsTrueWhenUnset(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":2,"tools":{"web":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if !cfg.Tools.Web.PreferNative {
		t.Fatal("PreferNative should remain true when unset in config file")
	}
}

func TestLoadConfig_SyntaxErrorReportsLineAndColumn(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := "{\n  \"version\": 2,\n  \"tools\": {\n    \"web\": {\n      \"enabled\": true,,\n      \"format\": \"markdown\"\n    }\n  }\n}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "syntax error at line 5, column 23") {
		t.Fatalf("expected line/column diagnostic, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "\"enabled\": true,,") {
		t.Fatalf("expected source snippet in diagnostic, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "^") {
		t.Fatalf("expected caret marker in diagnostic, got %q", err.Error())
	}
}

func TestLoadConfig_TypeErrorReportsFieldPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := "{\n  \"version\": 2,\n  \"tools\": {\n    \"web\": {\n      \"fetch_limit_bytes\": \"oops\"\n    }\n  }\n}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("expected type error, got nil")
	}
	if !strings.Contains(err.Error(), "type error at line 5, column 33") {
		t.Fatalf("expected line/column diagnostic, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "fetch_limit_bytes") {
		t.Fatalf("expected field name in diagnostic, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "\"fetch_limit_bytes\": \"oops\"") {
		t.Fatalf("expected source snippet in diagnostic, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "^") {
		t.Fatalf("expected caret marker in diagnostic, got %q", err.Error())
	}
}

func TestLoadConfig_UnknownFieldsReportsExactPaths(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := "{\n  \"version\": 2,\n  \"tools\": {\n    \"weeb\": {\n      \"enabled\": true\n    },\n    \"web\": {\n      \"fatch_limit_bytes\": 123\n    }\n  }\n}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("expected unknown field error, got nil")
	}
	if !strings.Contains(err.Error(), "tools.weeb") || !strings.Contains(err.Error(), "tools.web.fatch_limit_bytes") {
		t.Fatalf("expected exact unknown field paths, got %q", err.Error())
	}
}

func TestLoadConfig_UnderscoreFieldsIgnored(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// Fields starting with "_" should be ignored (like comments/metadata)
	raw := "{\n  \"version\": 2,\n  \"_version_fix_test\": \"some value\",\n  \"_comment\": \"this is a comment\",\n  \"tools\": {\n    \"_test_timestamp_fix\": true\n  }\n}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("expected underscore fields to be ignored, got error: %v", err)
	}
}

func TestDefaultConfig_ExecAllowRemoteEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.Exec.AllowRemote {
		t.Fatal("DefaultConfig().Tools.Exec.AllowRemote should be true")
	}
}

func TestDefaultConfig_FilterSensitiveDataEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.FilterSensitiveData {
		t.Fatal("DefaultConfig().Tools.FilterSensitiveData should be true")
	}
}

func TestDefaultConfig_FilterMinLength(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Tools.FilterMinLength != 8 {
		t.Fatalf("DefaultConfig().Tools.FilterMinLength = %d, want 8", cfg.Tools.FilterMinLength)
	}
}

func TestDefaultConfig_LoadImageEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.LoadImage.Enabled {
		t.Fatal("DefaultConfig().Tools.LoadImage.Enabled should be true")
	}
	if !cfg.Tools.IsToolEnabled("load_image") {
		t.Fatal("DefaultConfig().Tools.IsToolEnabled(load_image) should be true")
	}
}

func TestLoadConfig_LoadImageCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := "{\n  \"version\": 2,\n  \"tools\": {\n    \"load_image\": {\n      \"enabled\": false\n    }\n  }\n}\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if cfg.Tools.LoadImage.Enabled {
		t.Fatal("LoadConfig().Tools.LoadImage.Enabled should be false")
	}
	if cfg.Tools.IsToolEnabled("load_image") {
		t.Fatal("LoadConfig().Tools.IsToolEnabled(load_image) should be false")
	}
}

func TestDefaultConfig_MessageMediaDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.Message.Enabled {
		t.Fatal("DefaultConfig().Tools.Message.Enabled should be true")
	}
	if cfg.Tools.Message.MediaEnabled {
		t.Fatal("DefaultConfig().Tools.Message.MediaEnabled should be false")
	}
}

func TestToolsConfig_GetFilterMinLength(t *testing.T) {
	tests := []struct {
		name     string
		minLen   int
		expected int
	}{
		{"zero returns default", 0, 8},
		{"negative returns default", -1, 8},
		{"positive returns value", 16, 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ToolsConfig{FilterMinLength: tt.minLen}
			if got := cfg.GetFilterMinLength(); got != tt.expected {
				t.Errorf("GetFilterMinLength() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestDefaultConfig_CronAllowCommandEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Tools.Cron.AllowCommand {
		t.Fatal("DefaultConfig().Tools.Cron.AllowCommand should be true")
	}
}

func TestDefaultConfig_CronCommandAllowedRemotesEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.Tools.Cron.CommandAllowedRemotes) != 0 {
		t.Fatalf(
			"DefaultConfig().Tools.Cron.CommandAllowedRemotes = %#v, want empty",
			cfg.Tools.Cron.CommandAllowedRemotes,
		)
	}
}

func TestDefaultConfig_HooksDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Hooks.Enabled {
		t.Fatal("DefaultConfig().Hooks.Enabled should be true")
	}
	if cfg.Hooks.Defaults.ObserverTimeoutMS != 500 {
		t.Fatalf("ObserverTimeoutMS = %d, want 500", cfg.Hooks.Defaults.ObserverTimeoutMS)
	}
	if cfg.Hooks.Defaults.InterceptorTimeoutMS != 5000 {
		t.Fatalf("InterceptorTimeoutMS = %d, want 5000", cfg.Hooks.Defaults.InterceptorTimeoutMS)
	}
	if cfg.Hooks.Defaults.ApprovalTimeoutMS != 60000 {
		t.Fatalf("ApprovalTimeoutMS = %d, want 60000", cfg.Hooks.Defaults.ApprovalTimeoutMS)
	}
}

func TestDefaultConfig_LogLevel(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Gateway.LogLevel != "fatal" {
		t.Errorf("LogLevel = %q, want \"fatal\"", cfg.Gateway.LogLevel)
	}
}

func TestLoadConfig_ExecAllowRemoteDefaultsTrueWhenUnset(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":2,"tools":{"exec":{"enable_deny_patterns":true}}}`),
		0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if !cfg.Tools.Exec.AllowRemote {
		t.Fatal("tools.exec.allow_remote should remain true when unset in config file")
	}
}

func TestLoadConfig_CronAllowCommandDefaultsTrueWhenUnset(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(
		configPath,
		[]byte(`{"version":2,"tools":{"cron":{"exec_timeout_minutes":5}}}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if !cfg.Tools.Cron.AllowCommand {
		t.Fatal("tools.cron.allow_command should remain true when unset in config file")
	}
}

func TestLoadConfig_CronCommandAllowedRemotes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(
		configPath,
		[]byte(`{"version":2,"tools":{"cron":{"command_allowed_remotes":["telegram:1234567890","discord"]}}}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	want := []string{"telegram:1234567890", "discord"}
	if len(cfg.Tools.Cron.CommandAllowedRemotes) != len(want) {
		t.Fatalf("CommandAllowedRemotes = %#v, want %#v", cfg.Tools.Cron.CommandAllowedRemotes, want)
	}
	for i := range want {
		if cfg.Tools.Cron.CommandAllowedRemotes[i] != want[i] {
			t.Fatalf("CommandAllowedRemotes = %#v, want %#v", cfg.Tools.Cron.CommandAllowedRemotes, want)
		}
	}
}

func TestLoadConfig_HooksProcessConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configJSON := `{
  "version": 2,
  "hooks": {
    "processes": {
      "review-gate": {
        "enabled": true,
        "transport": "stdio",
        "command": ["uvx", "chagent-hook-reviewer"],
        "dir": "/tmp/hooks",
        "env": {
          "HOOK_MODE": "rewrite"
        },
        "observe": ["turn_start", "turn_end"],
        "intercept": ["before_tool", "approve_tool"]
      }
    },
    "builtins": {
      "audit": {
        "enabled": true,
        "priority": 5,
        "config": {
          "label": "audit"
        }
      }
    }
  }
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}

	processCfg, ok := cfg.Hooks.Processes["review-gate"]
	if !ok {
		t.Fatal("expected review-gate process hook")
	}
	if !processCfg.Enabled {
		t.Fatal("expected review-gate process hook to be enabled")
	}
	if processCfg.Transport != "stdio" {
		t.Fatalf("Transport = %q, want stdio", processCfg.Transport)
	}
	if len(processCfg.Command) != 2 || processCfg.Command[0] != "uvx" {
		t.Fatalf("Command = %v", processCfg.Command)
	}
	if processCfg.Dir != "/tmp/hooks" {
		t.Fatalf("Dir = %q, want /tmp/hooks", processCfg.Dir)
	}
	if processCfg.Env["HOOK_MODE"] != "rewrite" {
		t.Fatalf("HOOK_MODE = %q, want rewrite", processCfg.Env["HOOK_MODE"])
	}
	if len(processCfg.Observe) != 2 || processCfg.Observe[1] != "turn_end" {
		t.Fatalf("Observe = %v", processCfg.Observe)
	}
	if len(processCfg.Intercept) != 2 || processCfg.Intercept[1] != "approve_tool" {
		t.Fatalf("Intercept = %v", processCfg.Intercept)
	}

	builtinCfg, ok := cfg.Hooks.Builtins["audit"]
	if !ok {
		t.Fatal("expected audit builtin hook")
	}
	if !builtinCfg.Enabled {
		t.Fatal("expected audit builtin hook to be enabled")
	}
	if builtinCfg.Priority != 5 {
		t.Fatalf("Priority = %d, want 5", builtinCfg.Priority)
	}
	if !strings.Contains(string(builtinCfg.Config), `"audit"`) {
		t.Fatalf("Config = %s", string(builtinCfg.Config))
	}
	if cfg.Hooks.Defaults.ApprovalTimeoutMS != 60000 {
		t.Fatalf("ApprovalTimeoutMS = %d, want 60000", cfg.Hooks.Defaults.ApprovalTimeoutMS)
	}
}

// TestDefaultConfig_SessionDimensions verifies the default session dimensions
// TestDefaultConfig_SummarizationThresholds verifies summarization defaults
func TestDefaultConfig_SummarizationThresholds(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Agents.Defaults.SummarizeMessageThreshold != 20 {
		t.Errorf(
			"SummarizeMessageThreshold = %d, want 20",
			cfg.Agents.Defaults.SummarizeMessageThreshold,
		)
	}
	if cfg.Agents.Defaults.SummarizeTokenPercent != 75 {
		t.Errorf("SummarizeTokenPercent = %d, want 75", cfg.Agents.Defaults.SummarizeTokenPercent)
	}
}

func TestDefaultConfig_SessionDimensions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ApplyDmScope()

	if cfg.Session.DmScope != "per-peer" {
		t.Errorf("Session.DmScope = %q, want per-peer", cfg.Session.DmScope)
	}
	if len(cfg.Session.Dimensions) != 1 || cfg.Session.Dimensions[0] != "sender" {
		t.Errorf("Session.Dimensions = %v, want [sender]", cfg.Session.Dimensions)
	}
}

func TestSessionConfig_ApplyDmScope(t *testing.T) {
	tests := []struct {
		name       string
		dmScope    string
		dimensions []string
		want       []string
	}{
		{
			name:    "per-channel-peer",
			dmScope: "per-channel-peer",
			want:    []string{"chat", "sender"},
		},
		{
			name:    "per-channel",
			dmScope: "per-channel",
			want:    []string{"chat"},
		},
		{
			name:    "per-peer",
			dmScope: "per-peer",
			want:    []string{"sender"},
		},
		{
			name:    "global",
			dmScope: "global",
			want:    nil,
		},
		{
			name:       "explicit dimensions take precedence",
			dmScope:    "per-channel-peer",
			dimensions: []string{"sender"},
			want:       []string{"sender"},
		},
		{
			name:    "empty dm_scope is no-op",
			dmScope: "",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SessionConfig{
				DmScope:    tt.dmScope,
				Dimensions: tt.dimensions,
			}
			s.ApplyDmScope()
			if len(s.Dimensions) != len(tt.want) {
				t.Fatalf("Dimensions = %v, want %v", s.Dimensions, tt.want)
			}
			for i, v := range tt.want {
				if s.Dimensions[i] != v {
					t.Errorf("Dimensions[%d] = %q, want %q", i, s.Dimensions[i], v)
				}
			}
		})
	}
}

func TestSessionConfig_DeriveDmScope(t *testing.T) {
	tests := []struct {
		name       string
		dimensions []string
		dmScope    string
		wantScope  string
	}{
		{
			name:       "per-channel-peer from dimensions",
			dimensions: []string{"chat", "sender"},
			wantScope:  "per-channel-peer",
		},
		{
			name:       "per-channel from dimensions",
			dimensions: []string{"chat"},
			wantScope:  "per-channel",
		},
		{
			name:       "per-peer from dimensions",
			dimensions: []string{"sender"},
			wantScope:  "per-peer",
		},
		{
			name:       "custom dimensions does not set scope",
			dimensions: []string{"chat", "extra"},
			wantScope:  "",
		},
		{
			name:       "empty dimensions does not set scope",
			dimensions: nil,
			wantScope:  "",
		},
		{
			name:       "existing dm_scope is not overwritten",
			dimensions: []string{"chat", "sender"},
			dmScope:    "per-channel",
			wantScope:  "per-channel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SessionConfig{
				DmScope:    tt.dmScope,
				Dimensions: tt.dimensions,
			}
			s.DeriveDmScope()
			if s.DmScope != tt.wantScope {
				t.Errorf("DmScope = %q, want %q", s.DmScope, tt.wantScope)
			}
		})
	}
}

func TestSessionConfig_ApplyDmScope_ClearsStaleDimensions(t *testing.T) {
	// Simulates the PATCH handler scenario: dm_scope changed but stale
	// dimensions remain from the old scope. After clearing dimensions,
	// ApplyDmScope should re-derive from the new dm_scope.
	tests := []struct {
		name    string
		dmScope string
		want    []string
	}{
		{
			name:    "per-channel-peer to per-channel",
			dmScope: "per-channel",
			want:    []string{"chat"},
		},
		{
			name:    "per-channel-peer to per-peer",
			dmScope: "per-peer",
			want:    []string{"sender"},
		},
		{
			name:    "per-channel-peer to global",
			dmScope: "global",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SessionConfig{
				DmScope:    tt.dmScope,
				Dimensions: []string{"chat", "sender"}, // stale from per-channel-peer
			}
			// Simulate what the PATCH handler does: clear dimensions when dm_scope changes
			s.Dimensions = nil
			s.ApplyDmScope()
			if len(s.Dimensions) != len(tt.want) {
				t.Fatalf("Dimensions = %v, want %v", s.Dimensions, tt.want)
			}
			for i, v := range tt.want {
				if s.Dimensions[i] != v {
					t.Errorf("Dimensions[%d] = %q, want %q", i, s.Dimensions[i], v)
				}
			}
		})
	}
}

func TestDefaultConfig_WorkspacePath_Default(t *testing.T) {
	t.Setenv("CHAGENT_HOME", "")

	var fakeHome string
	if runtime.GOOS == "windows" {
		fakeHome = `C:\tmp\home`
		t.Setenv("USERPROFILE", fakeHome)
	} else {
		fakeHome = "/tmp/home"
		t.Setenv("HOME", fakeHome)
	}

	cfg := DefaultConfig()
	want := filepath.Join(fakeHome, pkg.DefaultChagentHome, "workspace")

	if cfg.Agents.Defaults.Workspace != want {
		t.Errorf("Default workspace path = %q, want %q", cfg.Agents.Defaults.Workspace, want)
	}
}

func TestDefaultConfig_WorkspacePath_WithChagentHome(t *testing.T) {
	t.Setenv("CHAGENT_HOME", "/custom/chagent/home")

	cfg := DefaultConfig()
	want := filepath.Join("/custom/chagent/home", "workspace")

	if cfg.Agents.Defaults.Workspace != want {
		t.Errorf(
			"Workspace path with CHAGENT_HOME = %q, want %q",
			cfg.Agents.Defaults.Workspace,
			want,
		)
	}
}

func TestDefaultConfig_IsolationEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Isolation.Enabled {
		t.Fatal("DefaultConfig().Isolation.Enabled should be false")
	}
}

func TestConfig_UnmarshalIsolation(t *testing.T) {
	cfg := DefaultConfig()
	raw := []byte(`{
		"isolation": {
			"enabled": false,
			"expose_paths": [
				{"source":"/src","target":"/dst","mode":"ro"}
			]
		}
	}`)
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatalf("json.Unmarshal isolation config: %v", err)
	}
	if cfg.Isolation.Enabled {
		t.Fatal("Isolation.Enabled should be false after unmarshal")
	}
	if len(cfg.Isolation.ExposePaths) != 1 {
		t.Fatalf("ExposePaths len = %d, want 1", len(cfg.Isolation.ExposePaths))
	}
	if got := cfg.Isolation.ExposePaths[0]; got.Source != "/src" || got.Target != "/dst" || got.Mode != "ro" {
		t.Fatalf("ExposePaths[0] = %+v, want source=/src target=/dst mode=ro", got)
	}
}

// TestFlexibleStringSlice_UnmarshalText tests UnmarshalText with various comma separators
func TestFlexibleStringSlice_UnmarshalText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "English commas only",
			input:    "123,456,789",
			expected: []string{"123", "456", "789"},
		},
		{
			name:     "Chinese commas only",
			input:    "123，456，789",
			expected: []string{"123", "456", "789"},
		},
		{
			name:     "Mixed English and Chinese commas",
			input:    "123,456，789",
			expected: []string{"123", "456", "789"},
		},
		{
			name:     "Single value",
			input:    "123",
			expected: []string{"123"},
		},
		{
			name:     "Values with whitespace",
			input:    " 123 , 456 , 789 ",
			expected: []string{"123", "456", "789"},
		},
		{
			name:     "Empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "Only commas - English",
			input:    ",,",
			expected: []string{},
		},
		{
			name:     "Only commas - Chinese",
			input:    "，，",
			expected: []string{},
		},
		{
			name:     "Mixed commas with empty parts",
			input:    "123,,456，，789",
			expected: []string{"123", "456", "789"},
		},
		{
			name:     "Complex mixed values",
			input:    "user1@example.com，user2@test.com, admin@domain.org",
			expected: []string{"user1@example.com", "user2@test.com", "admin@domain.org"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FlexibleStringSlice
			err := f.UnmarshalText([]byte(tt.input))
			if err != nil {
				t.Fatalf("UnmarshalText(%q) error = %v", tt.input, err)
			}

			if tt.expected == nil {
				if f != nil {
					t.Errorf("UnmarshalText(%q) = %v, want nil", tt.input, f)
				}
				return
			}

			if len(f) != len(tt.expected) {
				t.Errorf(
					"UnmarshalText(%q) length = %d, want %d",
					tt.input,
					len(f),
					len(tt.expected),
				)
				return
			}

			for i, v := range tt.expected {
				if f[i] != v {
					t.Errorf("UnmarshalText(%q)[%d] = %q, want %q", tt.input, i, f[i], v)
				}
			}
		})
	}
}

// TestFlexibleStringSlice_UnmarshalText_EmptySliceConsistency tests nil vs empty slice behavior
func TestFlexibleStringSlice_UnmarshalText_EmptySliceConsistency(t *testing.T) {
	t.Run("Empty string returns nil", func(t *testing.T) {
		var f FlexibleStringSlice
		err := f.UnmarshalText([]byte(""))
		if err != nil {
			t.Fatalf("UnmarshalText error = %v", err)
		}
		if f != nil {
			t.Errorf("Empty string should return nil, got %v", f)
		}
	})

	t.Run("Commas only returns empty slice", func(t *testing.T) {
		var f FlexibleStringSlice
		err := f.UnmarshalText([]byte(",,,"))
		if err != nil {
			t.Fatalf("UnmarshalText error = %v", err)
		}
		if f == nil {
			t.Error("Commas only should return empty slice, not nil")
		}
		if len(f) != 0 {
			t.Errorf("Expected empty slice, got %v", f)
		}
	})
}

func TestFlexibleStringSlice_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "null",
			input:    `null`,
			expected: nil,
		},
		{
			name:     "single string",
			input:    `"Thinking..."`,
			expected: []string{"Thinking..."},
		},
		{
			name:     "single number",
			input:    `123`,
			expected: []string{"123"},
		},
		{
			name:     "string array",
			input:    `["Thinking...", "Still working..."]`,
			expected: []string{"Thinking...", "Still working..."},
		},
		{
			name:     "mixed array",
			input:    `["123", 456]`,
			expected: []string{"123", "456"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FlexibleStringSlice
			if err := json.Unmarshal([]byte(tt.input), &f); err != nil {
				t.Fatalf("json.Unmarshal(%s) error = %v", tt.input, err)
			}
			if tt.expected == nil {
				if f != nil {
					t.Fatalf("json.Unmarshal(%s) = %#v, want nil slice", tt.input, f)
				}
				return
			}
			if len(f) != len(tt.expected) {
				t.Fatalf("json.Unmarshal(%s) len = %d, want %d", tt.input, len(f), len(tt.expected))
			}
			for i, want := range tt.expected {
				if f[i] != want {
					t.Fatalf("json.Unmarshal(%s)[%d] = %q, want %q", tt.input, i, f[i], want)
				}
			}
		})
	}
}

// TestLoadConfig_WarnsForPlaintextAPIKey verifies that LoadConfig resolves a plaintext
// api_keys entry into memory but does NOT rewrite the config file. File writes are the sole
// responsibility of SaveConfig.
func TestLoadConfig_WarnsForPlaintextAPIKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	const original = `{"version":2,"model_list":[{"model_name":"test","model":"openai/gpt-4","api_key":"sk-plaintext"}]}`
	if err := os.WriteFile(cfgPath, []byte(original), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Setenv("CHAGENT_KEY_PASSPHRASE", "test-passphrase")
	t.Setenv("CHAGENT_SSH_KEY_PATH", "")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// In-memory value must be the resolved plaintext.
	if cfg.ModelList[0].APIKey() != "sk-plaintext" {
		t.Errorf("in-memory api_key = %q, want %q", cfg.ModelList[0].APIKey(), "sk-plaintext")
	}
	// The file on disk must remain unchanged — no need upgrade version
	raw, _ := os.ReadFile(cfgPath)
	if string(raw) != original {
		t.Errorf("LoadConfig must not modify the config file; got:\n%s", string(raw))
	}
}

// TestSaveConfig_EncryptsPlaintextAPIKey verifies that SaveConfig writes enc:// ciphertext
// to disk and that a subsequent LoadConfig decrypts it back to the original plaintext.

// TestLoadConfig_NoSealWithoutPassphrase verifies that api_key values are left
// unchanged when CHAGENT_KEY_PASSPHRASE is not set.

// TestLoadConfig_FileRefNotSealed verifies that file:// api_key references are not
// converted to enc:// values (they are resolved at runtime by the Resolver).

// TestSaveConfig_MixedKeys verifies that SaveConfig encrypts only plaintext api_keys
// and leaves already-encrypted (enc://) and file:// entries unchanged.

// TestLoadConfig_MixedKeys_NoPassphrase verifies that when CHAGENT_KEY_PASSPHRASE
// is not set, enc:// entries cause LoadConfig to return an error, while plaintext
// and file:// entries in the same config are not affected.

// TestSaveConfig_UsesPassphraseProvider verifies that SaveConfig encrypts plaintext
// api_keys using credential.PassphraseProvider() rather than os.Getenv directly.
// This matters for the launcher, which clears the environment variable and redirects
// PassphraseProvider to an in-memory SecureStore.

// TestLoadConfig_UsesPassphraseProvider verifies that LoadConfig decrypts enc:// keys
// using credential.PassphraseProvider() rather than os.Getenv directly.

func TestConfigParsesLogLevel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := `{"version":2,"gateway":{"log_level":"debug"}}`
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Gateway.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want \"debug\"", cfg.Gateway.LogLevel)
	}
}

func TestConfigLogLevelEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := `{"version":2}`
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// When config omits log_level, the DefaultConfig value ("fatal") is preserved.
	if cfg.Gateway.LogLevel != "fatal" {
		t.Errorf("LogLevel = %q, want \"fatal\"", cfg.Gateway.LogLevel)
	}
}

func TestResolveGatewayLogLevel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := `{"version":2,"gateway":{"log_level":"debug"}}`
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if got := ResolveGatewayLogLevel(cfgPath); got != "debug" {
		t.Fatalf("ResolveGatewayLogLevel() = %q, want %q", got, "debug")
	}
}

func TestResolveGatewayLogLevel_UsesEnvOverrideAndNormalizesInvalid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := `{"version":2,"gateway":{"log_level":"debug"}}`
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Setenv("CHAGENT_LOG_LEVEL", "warning")
	if got := ResolveGatewayLogLevel(cfgPath); got != "warn" {
		t.Fatalf("ResolveGatewayLogLevel() with env override = %q, want %q", got, "warn")
	}

	t.Setenv("CHAGENT_LOG_LEVEL", "garbage")
	if got := ResolveGatewayLogLevel(cfgPath); got != DefaultGatewayLogLevel {
		t.Fatalf("ResolveGatewayLogLevel() with invalid env override = %q, want %q", got, DefaultGatewayLogLevel)
	}
}

func TestModelConfig_ExtraBodyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Version: CurrentVersion,
		ModelList: []*ModelConfig{
			{
				ModelName: "test-model",
				Model:     "openai/test",
				APIKeys:   SimpleSecureStrings("sk-test"),
				ExtraBody: map[string]any{"custom_field": "value", "num_field": 42},
			},
		},
	}

	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}

	if loaded.ModelList[0].ExtraBody == nil {
		t.Fatal("ExtraBody should not be nil after round-trip")
	}
	if got := loaded.ModelList[0].ExtraBody["custom_field"]; got != "value" {
		t.Errorf("ExtraBody[custom_field] = %v, want value", got)
	}
	if got := loaded.ModelList[0].ExtraBody["num_field"]; got != float64(42) {
		t.Errorf("ExtraBody[num_field] = %v, want 42", got)
	}
}

func TestModelConfig_CustomHeadersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Version: CurrentVersion,
		ModelList: []*ModelConfig{
			{
				ModelName:     "test-model",
				Model:         "openai/test",
				APIKeys:       SimpleSecureStrings("sk-test"),
				CustomHeaders: map[string]string{"X-Source": "coding-plan", "X-Agent": "openclaw"},
			},
		},
	}

	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}

	if loaded.ModelList[0].CustomHeaders == nil {
		t.Fatal("CustomHeaders should not be nil after round-trip")
	}
	if got := loaded.ModelList[0].CustomHeaders["X-Source"]; got != "coding-plan" {
		t.Errorf("CustomHeaders[X-Source] = %q, want coding-plan", got)
	}
	if got := loaded.ModelList[0].CustomHeaders["X-Agent"]; got != "openclaw" {
		t.Errorf("CustomHeaders[X-Agent] = %q, want openclaw", got)
	}
}

func TestModelConfig_ToolSchemaTransformRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Version: CurrentVersion,
		ModelList: []*ModelConfig{
			{
				ModelName:           "test-model",
				Model:               "openai/test",
				APIKeys:             SimpleSecureStrings("sk-test"),
				ToolSchemaTransform: "simple",
			},
		},
	}

	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}

	if got := loaded.ModelList[0].ToolSchemaTransform; got != "simple" {
		t.Fatalf("ToolSchemaTransform = %q, want %q", got, "simple")
	}
}

func TestDefaultConfig_MinimaxExtraBody(t *testing.T) {
	cfg := DefaultConfig()

	var minimaxCfg *ModelConfig
	for i := range cfg.ModelList {
		if cfg.ModelList[i].Provider == "minimax" && cfg.ModelList[i].Model == "MiniMax-M2.5" {
			minimaxCfg = cfg.ModelList[i]
			break
		}
	}
	if minimaxCfg == nil {
		t.Fatal("Minimax model not found in ModelList")
	}
	if minimaxCfg.ExtraBody == nil {
		t.Fatal("Minimax ExtraBody should not be nil")
	}
	if got, ok := minimaxCfg.ExtraBody["reasoning_split"]; !ok || got != true {
		t.Fatalf("Minimax ExtraBody[reasoning_split] = %v, want true", got)
	}
}

func TestFilterSensitiveData(t *testing.T) {
	// Test with nil security config
	cfg := &Config{}
	if got := cfg.FilterSensitiveData("hello sk-key123 world"); got != "hello sk-key123 world" {
		t.Errorf("nil security: got %q, want original", got)
	}

	// Test with empty content
	if got := cfg.FilterSensitiveData(""); got != "" {
		t.Errorf("empty content: got %q, want empty", got)
	}

	// Test short content (less than FilterMinLength=8, should skip filtering)
	cfg.ModelList = SecureModelList{
		&ModelConfig{
			ModelName: "test",
			APIKeys:   SimpleSecureStrings("sk-long-key-12345"),
		},
	}
	m, err := cfg.GetModelConfig("test")
	assert.NoError(t, err)
	m.APIKeys = SimpleSecureStrings("sk-long-key-12345")
	cfg.Tools.FilterSensitiveData = true
	cfg.Tools.FilterMinLength = 8

	// Debug: check if sensitive values are collected
	values := cfg.collectSensitiveValues()
	t.Logf("collected %d sensitive values: %v", len(values), values)

	if got := cfg.FilterSensitiveData("sk-key"); got != "sk-key" {
		t.Errorf("short content should not be filtered: got %q", got)
	}

	// Test filtering works
	content := "Your API key is sk-long-key-12345 and token abc123"
	// abc123 is not in sensitive values, only sk-long-key-12345 should be filtered
	expected := "Your API key is [FILTERED] and token abc123"
	if got := cfg.FilterSensitiveData(content); got != expected {
		t.Errorf("filtering failed: got %q, want %q", got, expected)
	}

	// Test disabled filtering
	cfg.Tools.FilterSensitiveData = false
	if got := cfg.FilterSensitiveData(content); got != content {
		t.Errorf("disabled filtering: got %q, want original %q", got, content)
	}
}

func TestFilterSensitiveData_MultipleKeys(t *testing.T) {
	cfg := &Config{
		Tools: ToolsConfig{
			FilterSensitiveData: true,
			FilterMinLength:     8,
		},
		ModelList: SecureModelList{
			&ModelConfig{
				ModelName: "model1",
				Model:     "openai/model1",
				APIKeys:   SecureStrings{NewSecureString("key-one"), NewSecureString("key-two")},
			},
			&ModelConfig{
				ModelName: "model2",
				Model:     "openai/model2",
				APIKeys:   SecureStrings{NewSecureString("key-three")},
			},
		},
	}

	content := "key-one and key-two and key-three should be filtered"
	expected := "[FILTERED] and [FILTERED] and [FILTERED] should be filtered"
	if got := cfg.FilterSensitiveData(content); got != expected {
		t.Errorf("multiple keys: got %q, want %q", got, expected)
	}
}

func TestFilterSensitiveData_AllTokenTypes(t *testing.T) {
	cfg := &Config{
		// Model API keys
		ModelList: SecureModelList{
			&ModelConfig{
				ModelName: "test-model",
				APIKeys:   SecureStrings{NewSecureString("sk-model-key-12345")},
			},
		},
		// Channel tokens
		Channels: testChannelsConfigWithTokens(),
		Tools: ToolsConfig{
			FilterSensitiveData: true,
			FilterMinLength:     8,
			// Web tool API keys
			Web: WebToolsConfig{
				Brave: BraveConfig{APIKeys: SecureStrings{NewSecureString("brave-api-key")}},
				Tavily: TavilyConfig{
					APIKeys: SecureStrings{NewSecureString("tavily-api-key")},
				},
				Perplexity: PerplexityConfig{
					APIKeys: SecureStrings{NewSecureString("perplexity-api-key")},
				},
				GLMSearch:   GLMSearchConfig{APIKey: *NewSecureString("glm-search-key")},
				BaiduSearch: BaiduSearchConfig{APIKey: *NewSecureString("baidu-search-key")},
			},
			// Skills tokens
			Skills: SkillsToolsConfig{
				Github: SkillsGithubConfig{Token: *NewSecureString("github-token-xyz")},
				Registries: SkillsRegistriesConfig{
					&SkillRegistryConfig{Name: "clawhub", AuthToken: *NewSecureString("clawhub-auth-token")},
				},
			},
		},
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "model_api_key",
			content: "Using model with key sk-model-key-12345",
			want:    "Using model with key [FILTERED]",
		},
		{
			name:    "brave_api_key",
			content: "Brave key: brave-api-key",
			want:    "Brave key: [FILTERED]",
		},
		{
			name:    "tavily_api_key",
			content: "Tavily key: tavily-api-key",
			want:    "Tavily key: [FILTERED]",
		},
		{
			name:    "github_token",
			content: "GitHub token: github-token-xyz",
			want:    "GitHub token: [FILTERED]",
		},
		{
			name:    "mixed_content",
			content: "Model key sk-model-key-12345 and Pico token pico-token-abc123",
			want:    "Model key [FILTERED] and Pico token [FILTERED]",
		},
		{
			name:    "short_key_not_filtered",
			content: "Key abc not filtered because length < 8",
			want:    "Key abc not filtered because length < 8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.FilterSensitiveData(tt.content); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MakeBackup tests
// ---------------------------------------------------------------------------

// TestMakeBackup_WithDateSuffix verifies backup files include a date suffix.
func TestMakeBackup_WithDateSuffix(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := MakeBackup(configPath); err != nil {
		t.Fatalf("MakeBackup: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var hasDatedBackup bool
	for _, e := range entries {
		if matched, _ := filepath.Match("config.json.20*.bak", e.Name()); matched {
			hasDatedBackup = true
			// Verify backup content matches original
			bakPath := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(bakPath)
			if err != nil {
				t.Fatalf("ReadFile backup: %v", err)
			}
			if string(data) != `{"version":2}` {
				t.Errorf("backup content = %q, want original content", string(data))
			}
			break
		}
	}
	if !hasDatedBackup {
		t.Error("expected backup file with date suffix pattern config.json.20*.bak")
	}
}

// TestMakeBackup_AlsoBacksSecurityFile verifies that the security config file
// is also backed up with the same date suffix.

// TestMakeBackup_NonexistentFileSkipsBackup verifies that MakeBackup returns nil
// when the config file does not exist (no error, no panic).
func TestMakeBackup_NonexistentFileSkipsBackup(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	if err := MakeBackup(configPath); err != nil {
		t.Fatalf("MakeBackup on nonexistent file should return nil, got: %v", err)
	}
}

// TestMakeBackup_OnlyConfigNoSecurity verifies backup succeeds when only
// the config file exists and no security file.
func TestMakeBackup_OnlyConfigNoSecurity(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{"version":2}`), 0o600)

	if err := MakeBackup(configPath); err != nil {
		t.Fatalf("MakeBackup: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	configBackups := 0
	secBackups := 0
	for _, e := range entries {
		if matched, _ := filepath.Match("config.json.20*.bak", e.Name()); matched {
			configBackups++
		}
		if matched, _ := filepath.Match(".security.yml.20*.bak", e.Name()); matched {
			secBackups++
		}
	}
	if configBackups != 1 {
		t.Errorf("expected 1 config backup, got %d", configBackups)
	}
	if secBackups != 0 {
		t.Errorf("expected 0 security backups when no security file exists, got %d", secBackups)
	}
}

// TestMakeBackup_SameDateSuffix verifies that config and security backups
// share the same date suffix (they are created in the same MakeBackup call).

func testChannelsConfigWithTokens() ChannelsConfig {
	channels := make(ChannelsConfig)
	bc := &Channel{Type: ChannelPico}
	bc.Decode(PicoSettings{Token: *NewSecureString("pico-token-abc123")})
	channels["pico"] = bc
	return channels
}
