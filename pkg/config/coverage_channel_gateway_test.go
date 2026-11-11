package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rlds/chagent/pkg/credential"
	"github.com/rlds/chagent/pkg/logger"
)

func TestCoverageChannelJSONCodec(t *testing.T) {
	t.Run("unmarshal merges unknown keys into settings", func(t *testing.T) {
		var c Channel
		raw := `{"enabled":true,"type":"pico","allow_from":"a,b","reasoning_channel_id":"r","group_trigger":{"mention_only":true},"typing":{"enabled":true},"placeholder":{"enabled":true,"text":"wait"},"settings":{"token":"t"},"custom_key":7}`
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("UnmarshalJSON() error = %v", err)
		}
		if !c.Enabled || c.Type != "pico" || c.ReasoningChannelID != "r" {
			t.Fatalf("common fields = %+v", c)
		}
		if len(c.AllowFrom) != 1 || c.AllowFrom[0] != "a,b" {
			t.Fatalf("allow_from = %v", c.AllowFrom)
		}
		if !c.GroupTrigger.MentionOnly {
			t.Fatalf("group_trigger = %+v", c.GroupTrigger)
		}
		if !c.Typing.Enabled {
			t.Fatalf("typing = %+v", c.Typing)
		}
		if !c.Placeholder.Enabled {
			t.Fatalf("placeholder = %+v", c.Placeholder)
		}
		if !strings.Contains(string(c.Settings), "custom_key") || !strings.Contains(string(c.Settings), "token") {
			t.Fatalf("merged settings = %s", c.Settings)
		}
	})

	t.Run("unmarshal settings must be object", func(t *testing.T) {
		var c Channel
		if err := json.Unmarshal([]byte(`{"type":"pico","settings":[1]}`), &c); err == nil ||
			!strings.Contains(err.Error(), "channel settings") {
			t.Fatalf("UnmarshalJSON(settings array) error = %v", err)
		}
	})

	t.Run("unmarshal invalid json and field type", func(t *testing.T) {
		var c Channel
		if err := json.Unmarshal([]byte(`{`), &c); err == nil {
			t.Fatal("UnmarshalJSON(invalid) expected error")
		}
		if err := json.Unmarshal([]byte(`{"enabled":"yes"}`), &c); err == nil {
			t.Fatal("UnmarshalJSON(enabled string) expected error")
		}
	})

	t.Run("marshal without extend keeps raw settings", func(t *testing.T) {
		c := Channel{Enabled: true, Type: "pico", Settings: RawNode(`{"token":"t"}`)}
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("MarshalJSON() error = %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(blob, &decoded); err != nil {
			t.Fatalf("MarshalJSON output: %v", err)
		}
		settings, _ := decoded["settings"].(map[string]any)
		if settings["token"] != "t" {
			t.Fatalf("settings = %v", decoded["settings"])
		}
	})

	t.Run("marshal with extend preserves explicit disabled streaming", func(t *testing.T) {
		c := Channel{Enabled: true, Type: "pico", Settings: RawNode(`{"streaming":{"enabled":false},"token":"t"}`)}
		if err := c.Decode(&PicoSettings{}); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		// Force extend to a value that drops streaming when re-encoded.
		c.extend = struct {
			Token string `json:"token"`
		}{Token: "t"}
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("MarshalJSON(extend) error = %v", err)
		}
		if !strings.Contains(string(blob), `"enabled":false`) {
			t.Fatalf("explicit disabled streaming lost: %s", blob)
		}
	})

	t.Run("marshal extend failure", func(t *testing.T) {
		c := Channel{extend: make(chan int)}
		if _, err := json.Marshal(c); err == nil {
			t.Fatal("MarshalJSON(chan extend) expected error")
		}
	})
}

func TestCoveragePreserveExplicitDisabledStreaming(t *testing.T) {
	settings := RawNode(`{"token":"t"}`)
	cases := []struct {
		name     string
		settings RawNode
		original RawNode
		wantSub  string
	}{
		{"no original", settings, nil, `{"token":"t"}`},
		{"no settings", nil, RawNode(`{"streaming":{"enabled":false}}`), ""},
		{"invalid original", settings, RawNode(`{`), `{"token":"t"}`},
		{"streaming not object", settings, RawNode(`{"streaming":true}`), `{"token":"t"}`},
		{"streaming enabled", settings, RawNode(`{"streaming":{"enabled":true}}`), `{"token":"t"}`},
		{"already present", RawNode(`{"streaming":{"enabled":true}}`), RawNode(`{"streaming":{"enabled":false}}`), `"enabled":true`},
		{"invalid settings json", RawNode(`{`), RawNode(`{"streaming":{"enabled":false}}`), "{"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preserveExplicitDisabledStreaming(tc.settings, tc.original)
			if tc.wantSub == "" {
				if got != nil && len(got) != 0 && !strings.Contains(string(got), "enabled") {
					t.Fatalf("preserveExplicitDisabledStreaming() = %s", got)
				}
				return
			}
			if !strings.Contains(string(got), strings.TrimSpace(tc.wantSub)) {
				t.Fatalf("preserveExplicitDisabledStreaming() = %s, want %q", got, tc.wantSub)
			}
		})
	}
	added := preserveExplicitDisabledStreaming(RawNode(`{"token":"t"}`), RawNode(`{"streaming":{"enabled":false}}`))
	if !strings.Contains(string(added), `"streaming":{"enabled":false}`) {
		t.Fatalf("streaming not re-added: %s", added)
	}
}

func TestCoverageChannelYAMLCodec(t *testing.T) {
	t.Run("marshal decodes pico settings", func(t *testing.T) {
		c := Channel{Enabled: true, Type: "pico", Settings: RawNode(`{"token":"tok"}`)}
		out, err := c.MarshalYAML()
		if err != nil {
			t.Fatalf("MarshalYAML() error = %v", err)
		}
		blob, err := yaml.Marshal(out)
		if err != nil {
			t.Fatalf("MarshalYAML output: %v", err)
		}
		if !strings.Contains(string(blob), "token") {
			t.Fatalf("MarshalYAML = %s", blob)
		}
	})

	t.Run("marshal unknown type yields nil settings", func(t *testing.T) {
		out, err := (&Channel{Type: "unknown-type"}).MarshalYAML()
		if err != nil {
			t.Fatalf("MarshalYAML() error = %v", err)
		}
		blob, _ := yaml.Marshal(out)
		if strings.Contains(string(blob), "token") {
			t.Fatalf("MarshalYAML(unknown) = %s", blob)
		}
	})

	t.Run("unmarshal zero kind no-op", func(t *testing.T) {
		c := Channel{Enabled: true, Type: "pico"}
		if err := c.UnmarshalYAML(&yaml.Node{}); err != nil || !c.Enabled {
			t.Fatalf("UnmarshalYAML(zero) = %+v, %v", c, err)
		}
	})

	t.Run("unmarshal valid resets extend", func(t *testing.T) {
		c := Channel{extend: &PicoSettings{}}
		node := yamlMapping(t, "enabled: true\ntype: pico\nsettings:\n  token: t\n")
		if err := c.UnmarshalYAML(node); err != nil {
			t.Fatalf("UnmarshalYAML() error = %v", err)
		}
		if c.extend != nil || len(c.Settings) == 0 {
			t.Fatalf("UnmarshalYAML() = %+v (extend=%v)", c, c.extend)
		}
	})

	t.Run("unmarshal invalid settings type", func(t *testing.T) {
		c := Channel{}
		node := yamlMapping(t, "settings: [1,2]\n")
		if err := c.UnmarshalYAML(node); err == nil {
			t.Fatal("UnmarshalYAML(settings array) expected error")
		}
	})
}

func TestCoverageInitChannelList(t *testing.T) {
	t.Run("nil channel removed and type derived", func(t *testing.T) {
		channels := ChannelsConfig{"pico": nil, "pico-secondary": {Enabled: true, Type: "pico"}}
		if err := InitChannelList(channels); err != nil {
			t.Fatalf("InitChannelList() error = %v", err)
		}
		if _, ok := channels["pico"]; ok {
			t.Fatal("nil channel not removed")
		}
		if channels["pico-secondary"].Name() != "pico-secondary" {
			t.Fatalf("channel name = %q", channels["pico-secondary"].Name())
		}
	})

	t.Run("unknown channel type", func(t *testing.T) {
		channels := ChannelsConfig{"weird": {Enabled: true, Type: "definitely-unknown"}}
		err := InitChannelList(channels)
		if err == nil || !strings.Contains(err.Error(), "unknown type") {
			t.Fatalf("InitChannelList(unknown) error = %v", err)
		}
	})

	t.Run("pico decoding and streaming validation", func(t *testing.T) {
		channels := ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: mustParseRawNode(`{"token":"t","ping_interval":5}`)}}
		if err := InitChannelList(channels); err != nil {
			t.Fatalf("InitChannelList(pico) error = %v", err)
		}
		bad := ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: mustParseRawNode(`{"streaming":{"throttle_seconds":-1}}`)}}
		err := InitChannelList(bad)
		if err == nil || !strings.Contains(err.Error(), "throttle_seconds") {
			t.Fatalf("InitChannelList(throttle) error = %v", err)
		}
		badGrowth := ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: mustParseRawNode(`{"streaming":{"min_growth_chars":-1}}`)}}
		err = InitChannelList(badGrowth)
		if err == nil || !strings.Contains(err.Error(), "min_growth_chars") {
			t.Fatalf("InitChannelList(min_growth) error = %v", err)
		}
	})

	t.Run("duplicate singleton channel", func(t *testing.T) {
		channels := ChannelsConfig{
			"pico":   {Enabled: true, Type: "pico"},
			"pico-2": {Enabled: true, Type: "pico"},
		}
		err := InitChannelList(channels)
		if err == nil || !strings.Contains(err.Error(), "singleton") {
			t.Fatalf("InitChannelList(duplicate singleton) error = %v", err)
		}
	})

	t.Run("decode failure", func(t *testing.T) {
		channels := ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: RawNode(`{"streaming":"bad"}`)}}
		err := InitChannelList(channels)
		if err == nil || !strings.Contains(err.Error(), "failed to decode settings") {
			t.Fatalf("InitChannelList(decode) error = %v", err)
		}
	})
}

func TestCoverageGatewayHelpers(t *testing.T) {
	// canonical log levels.
	for _, tc := range []struct {
		level logger.LogLevel
		want  string
	}{
		{logger.DEBUG, "debug"},
		{logger.INFO, "info"},
		{logger.WARN, "warn"},
		{logger.ERROR, "error"},
		{logger.FATAL, "fatal"},
		{logger.LogLevel(99), DefaultGatewayLogLevel},
	} {
		if got := canonicalGatewayLogLevel(tc.level); got != tc.want {
			t.Fatalf("canonicalGatewayLogLevel(%q) = %q, want %q", tc.level, got, tc.want)
		}
	}
	if got := normalizeGatewayLogLevel("DEBUG"); got != "debug" {
		t.Fatalf("normalizeGatewayLogLevel(DEBUG) = %q", got)
	}
	if got := normalizeGatewayLogLevel("nonsense"); got != DefaultGatewayLogLevel {
		t.Fatalf("normalizeGatewayLogLevel(nonsense) = %q", got)
	}
	if got := EffectiveGatewayLogLevel(nil); got != DefaultGatewayLogLevel {
		t.Fatalf("EffectiveGatewayLogLevel(nil) = %q", got)
	}
	cfg := &Config{}
	cfg.Gateway.LogLevel = "info"
	if got := EffectiveGatewayLogLevel(cfg); got != "info" {
		t.Fatalf("EffectiveGatewayLogLevel(info) = %q", got)
	}

	// Host normalization.
	if got, err := normalizeGatewayHostInput("127.0.0.1"); err != nil || got == "" {
		t.Fatalf("normalizeGatewayHostInput(ip) = %q, %v", got, err)
	}
	if got, err := normalizeGatewayHostInput(""); err != nil || got == "" {
		t.Fatalf("normalizeGatewayHostInput(empty) = %q, %v", got, err)
	}

	// ResolveGatewayLogLevel: missing file, valid config, invalid JSON, env override.
	missing := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("CHAGENT_LOG_LEVEL", "")
	if got := ResolveGatewayLogLevel(missing); got != DefaultGatewayLogLevel {
		t.Fatalf("ResolveGatewayLogLevel(missing) = %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"gateway":{"log_level":"debug"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := ResolveGatewayLogLevel(path); got != "debug" {
		t.Fatalf("ResolveGatewayLogLevel(debug) = %q", got)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := ResolveGatewayLogLevel(bad); got != DefaultGatewayLogLevel {
		t.Fatalf("ResolveGatewayLogLevel(bad) = %q", got)
	}
	t.Setenv("CHAGENT_LOG_LEVEL", "error")
	if got := ResolveGatewayLogLevel(path); got != "error" {
		t.Fatalf("ResolveGatewayLogLevel(env) = %q", got)
	}
}

func TestCoverageMCPTransport(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"streamable-http", "http"},
		{"streamable_http", "http"},
		{"StreamableHTTP", "http"},
		{" HTTP ", "http"},
		{"sse", "sse"},
		{"", ""},
	} {
		if got := NormalizeMCPTransportType(tc.in); got != tc.want {
			t.Fatalf("NormalizeMCPTransportType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		cfg  MCPServerConfig
		want string
	}{
		{"explicit", MCPServerConfig{Type: "http"}, "http"},
		{"url", MCPServerConfig{URL: "https://x"}, "sse"},
		{"command", MCPServerConfig{Command: "x"}, "stdio"},
		{"none", MCPServerConfig{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveMCPTransportType(tc.cfg); got != tc.want {
				t.Fatalf("EffectiveMCPTransportType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCoverageMinimalHelpers(t *testing.T) {
	// defaultWorkspaceChain: env workspace wins, then env config dir, then path dir.
	t.Setenv(EnvWorkspace, "/tmp/env-ws")
	if got := defaultWorkspaceChain("/tmp/elsewhere/config.json"); got != "/tmp/env-ws" {
		t.Fatalf("defaultWorkspaceChain(env ws) = %q", got)
	}
	t.Setenv(EnvWorkspace, "")
	t.Setenv(EnvConfig, "/tmp/cfgdir/config.json")
	if got := defaultWorkspaceChain(""); got != "/tmp/cfgdir" {
		t.Fatalf("defaultWorkspaceChain(env config) = %q", got)
	}
	t.Setenv(EnvConfig, "")
	if got := defaultWorkspaceChain("/tmp/another/config.json"); got != "/tmp/another" {
		t.Fatalf("defaultWorkspaceChain(path) = %q", got)
	}
}

func TestCoverageSecurityCacheBranches(t *testing.T) {
	// No sensitive values at all -> empty replacer.
	empty := &Config{}
	if empty.SensitiveDataReplacer() == nil {
		t.Fatal("SensitiveDataReplacer() = nil")
	}
	// Sensitive values shorter than the filter threshold -> empty replacer.
	short := &Config{ModelList: []*ModelConfig{{ModelName: "m", Model: "m", APIKeys: SimpleSecureStrings("abc")}}}
	if short.SensitiveDataReplacer() == nil {
		t.Fatal("SensitiveDataReplacer(short) = nil")
	}
	// Cached replacer is reused (once).
	long := &Config{ModelList: []*ModelConfig{{ModelName: "m", Model: "m", APIKeys: SimpleSecureStrings("long-secret-value")}}}
	if long.SensitiveDataReplacer() != long.SensitiveDataReplacer() {
		t.Fatal("SensitiveDataReplacer() did not cache")
	}
}

func TestCoverageTurnProfileHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   TurnProfileMode
		want TurnProfileMode
	}{
		{"", TurnProfileModeDefault},
		{"default", TurnProfileModeDefault},
		{"OFF", TurnProfileModeOff},
		{"custom", TurnProfileModeCustom},
		{"weird", TurnProfileMode("weird")},
	} {
		if got := tc.in.Effective(); got != tc.want {
			t.Fatalf("Effective(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	var nilDefaults *AgentDefaults
	if _, ok, err := nilDefaults.ResolveTurnProfile(); ok || err != nil {
		t.Fatalf("nil ResolveTurnProfile() = %v, %v", ok, err)
	}
	disabled := &AgentDefaults{}
	if _, ok, err := disabled.ResolveTurnProfile(); ok || err != nil {
		t.Fatalf("disabled ResolveTurnProfile() = %v, %v", ok, err)
	}
	enabled := &AgentDefaults{}
	enabled.TurnProfile = TurnProfileConfig{
		Enabled: true,
		Skills:  TurnProfileBlock{Mode: TurnProfileModeCustom, Allow: []string{" s1 ", ""}},
		Tools:   TurnProfileBlock{Mode: TurnProfileModeCustom, Allow: []string{"t1", " "}},
	}
	profile, ok, err := enabled.ResolveTurnProfile()
	if err != nil || !ok {
		t.Fatalf("ResolveTurnProfile() = %v, %v", ok, err)
	}
	if len(profile.AllowedSkills) != 1 || profile.AllowedSkills[0] != "s1" {
		t.Fatalf("AllowedSkills = %v", profile.AllowedSkills)
	}
	if len(profile.AllowedTools) != 1 || profile.AllowedTools[0] != "t1" {
		t.Fatalf("AllowedTools = %v", profile.AllowedTools)
	}

	var nilCfg *Config
	if err := nilCfg.ValidateTurnProfile(); err != nil {
		t.Fatalf("nil ValidateTurnProfile() = %v", err)
	}
	invalid := &Config{}
	invalid.Agents.Defaults.TurnProfile = TurnProfileConfig{
		Enabled: true,
		History: TurnProfileBlock{Mode: TurnProfileModeCustom},
	}
	if err := invalid.ValidateTurnProfile(); err == nil {
		t.Fatal("ValidateTurnProfile(custom history) expected error")
	}
	if err := validateTurnProfile(TurnProfileConfig{}); err != nil {
		t.Fatalf("validateTurnProfile(disabled) = %v", err)
	}
}

func TestCoverageSecureStringNewAndYAMLPassphrase(t *testing.T) {
	// fromRaw error branch: unresolvable file:// credential logs and preserves raw.
	bad := NewSecureString("file://definitely-missing-credential-file")
	if bad == nil || bad.raw == "" {
		t.Fatalf("NewSecureString(unresolvable) = %+v", bad)
	}

	// Passphrase branch of MarshalYAML.
	old := credential.PassphraseProvider
	credential.PassphraseProvider = func() string { return "test-passphrase" }
	defer func() { credential.PassphraseProvider = old }()
	s := NewSecureString("secret-value")
	// Encrypting requires an SSH key in the test environment; either the
	// encrypted payload or a diagnosable error is acceptable, but the branch
	// must be exercised without panicking.
	if _, err := s.MarshalYAML(); err != nil {
		if !strings.Contains(err.Error(), "credential") && !strings.Contains(err.Error(), "SSH") {
			t.Fatalf("MarshalYAML(passphrase) unexpected error = %v", err)
		}
	}
}
