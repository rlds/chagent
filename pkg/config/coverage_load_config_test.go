package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg"
)

// writeRawConfigFile writes raw config JSON and points EnvConfig at it without
// setting the other environment knobs, so each test can control them.
func writeRawConfigFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvWorkspace, "")
	return path
}

func TestCoverageIsToolEnabled(t *testing.T) {
	cfg := DefaultConfig()
	tools := &cfg.Tools
	tools.Web.Enabled = true
	tools.Cron.Enabled = true
	tools.Exec.Enabled = true
	tools.Skills.Enabled = true
	tools.MediaCleanup.Enabled = true
	tools.AppendFile.Enabled = true
	tools.EditFile.Enabled = true
	tools.FindSkills.Enabled = true
	tools.InstallSkill.Enabled = true
	tools.ListDir.Enabled = true
	tools.LoadImage.Enabled = true
	tools.Message.Enabled = true
	tools.ReadFile.Enabled = true
	tools.Spawn.Enabled = true
	tools.SpawnStatus.Enabled = true
	tools.Subagent.Enabled = true
	tools.Task.Enabled = true
	tools.Todo.Enabled = true
	tools.SkillLoad.Enabled = true
	tools.WebFetch.Enabled = true
	tools.SendFile.Enabled = true
	tools.WriteFile.Enabled = true
	tools.MCP.Enabled = true
	enabled := true
	tools.DSH.Enabled = &enabled

	for _, name := range []string{
		"web", "cron", "exec", "skills", "media_cleanup", "append_file",
		"edit_file", "find_skills", "install_skill", "list_dir", "load_image",
		"message", "read_file", "spawn", "spawn_status", "subagent", "task",
		"task_get", "task_update", "todo", "todo_write", "skill", "skill_load",
		"skill_view", "web_fetch", "send_file", "write_file", "mcp", "dsh",
	} {
		if !tools.IsToolEnabled(name) {
			t.Fatalf("IsToolEnabled(%q) = false, want true", name)
		}
	}
	if !tools.IsToolEnabled("unknown-tool") {
		t.Fatal("unknown tool should default enabled")
	}

	off := &ToolsConfig{}
	for _, name := range []string{
		"web", "cron", "exec", "skills", "media_cleanup", "append_file",
		"edit_file", "find_skills", "install_skill", "list_dir", "load_image",
		"message", "read_file", "spawn", "spawn_status", "subagent", "task",
		"todo", "skill", "web_fetch", "send_file", "write_file", "mcp",
	} {
		if off.IsToolEnabled(name) {
			t.Fatalf("IsToolEnabled(%q) = true on zero config", name)
		}
	}
	// The DSH tool group is default-enabled when its switch is omitted.
	if !off.IsToolEnabled("dsh") {
		t.Fatal("IsToolEnabled(\"dsh\") = false on zero config, want default-enabled")
	}
}

func TestCoverageLoadConfigFileBranches(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		t.Setenv(EnvConfig, filepath.Join(t.TempDir(), "absent.json"))
		t.Setenv(EnvWorkspace, "")
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg == nil || cfg.Version != CurrentVersion {
			t.Fatalf("LoadConfig() = %+v, want default config", cfg)
		}
	})

	t.Run("tiny file returns defaults", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"a":1}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg == nil || cfg.Version != CurrentVersion {
			t.Fatalf("LoadConfig(tiny) = %+v", cfg)
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":1}`)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "unsupported config version: 1") {
			t.Fatalf("LoadConfig(version=1) error = %v", err)
		}
	})

	t.Run("plural api_keys rejected", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"model_list":[{"model_name":"m","model":"m","api_keys":["k"]}]}`)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "api_keys is not supported") {
			t.Fatalf("LoadConfig(plural api_keys) error = %v", err)
		}
	})

	t.Run("unknown top-level field", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"bogus":1}`)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("LoadConfig(unknown field) error = %v", err)
		}
	})

	t.Run("nested type error", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"agents":{"defaults":{"max_tokens":"oops"}}}`)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "max_tokens") {
			t.Fatalf("LoadConfig(type error) error = %v", err)
		}
	})

	t.Run("invalid turn profile", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"agents":{"defaults":{"turn_profile":{"enabled":true,"tools_mode":"bogus"}}}}`)
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("LoadConfig(invalid turn profile) expected error")
		}
	})

	t.Run("invalid skills catalog mode", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"tools":{"skills":{"catalog_mode":"sometimes"}}}`)
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("LoadConfig(invalid catalog_mode) expected error")
		}
	})

	t.Run("invalid model list", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"model_list":[{"model_name":"m"}]}`)
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), "model_list[0]") {
			t.Fatalf("LoadConfig(invalid model) error = %v", err)
		}
	})

	t.Run("explicit workspace in file is preserved", func(t *testing.T) {
		ws := filepath.Join(t.TempDir(), "ws")
		path := writeRawConfigFile(t, `{"version":2,"agents":{"defaults":{"workspace":`+quoteJSON(ws)+`}}}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Agents.Defaults.Workspace != ws {
			t.Fatalf("Workspace = %q, want %q", cfg.Agents.Defaults.Workspace, ws)
		}
	})

	t.Run("workspace defaults to config dir", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Agents.Defaults.Workspace != filepath.Dir(path) {
			t.Fatalf("Workspace = %q, want config dir %q", cfg.Agents.Defaults.Workspace, filepath.Dir(path))
		}
	})

	t.Run("workspace env counts as explicit", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2}`)
		t.Setenv("CHAGENT_AGENTS_DEFAULTS_WORKSPACE", filepath.Join(t.TempDir(), "envws"))
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		def := DefaultConfig()
		if cfg.Agents.Defaults.Workspace != def.Agents.Defaults.Workspace {
			t.Fatalf("Workspace = %q, want default %q (env explicit)", cfg.Agents.Defaults.Workspace, def.Agents.Defaults.Workspace)
		}
	})

	t.Run("agent name derives pico token", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2}`)
		t.Setenv(AgentNameEnv, "coverage-agent")
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		pico := cfg.Channels.Get("pico")
		if pico == nil {
			t.Fatal("pico channel not created for agent name")
		}
		decoded, err := pico.GetDecoded()
		if err != nil {
			t.Fatalf("pico GetDecoded() error = %v", err)
		}
		settings, ok := decoded.(*PicoSettings)
		if !ok || settings.Token.String() != DerivePicoToken("coverage-agent") {
			t.Fatalf("pico token = %+v, want derived", decoded)
		}
	})

	t.Run("explicit mcp disable", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"tools":{"mcp":{"enabled":false,"servers":{"s":{"type":"stdio","command":"x"}}}}}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if !cfg.Tools.MCP.ExplicitDisabled || cfg.Tools.MCP.Active() {
			t.Fatalf("ExplicitDisabled=%v Active=%v, want explicit disable", cfg.Tools.MCP.ExplicitDisabled, cfg.Tools.MCP.Active())
		}
	})
}

func quoteJSON(s string) string {
	blob, _ := json.Marshal(s)
	return string(blob)
}

func TestCoverageLoadConfigInference(t *testing.T) {
	t.Run("enabled model and provider prefix", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"model_list":[
			{"model_name":"m1","model":"deepseek/chat","enabled":false},
			{"model_name":"m2","model":"deepseek/deepseek-chat","enabled":true}
		]}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Agents.Defaults.ModelName != "m2" {
			t.Fatalf("ModelName = %q, want m2 (first enabled)", cfg.Agents.Defaults.ModelName)
		}
		if cfg.Agents.Defaults.Provider != "deepseek" {
			t.Fatalf("Provider = %q, want deepseek", cfg.Agents.Defaults.Provider)
		}
	})

	t.Run("explicit provider is preserved", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"agents":{"defaults":{"provider":"custom"}},"model_list":[
			{"model_name":"m","model":"openai/gpt","enabled":true}
		]}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Agents.Defaults.Provider != "custom" {
			t.Fatalf("Provider = %q, want custom", cfg.Agents.Defaults.Provider)
		}
	})

	t.Run("model without provider falls back to openai", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"model_list":[{"model_name":"m","model":"plain-model"}]}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Agents.Defaults.ModelName != "m" || cfg.Agents.Defaults.Provider != "openai" {
			t.Fatalf("inference = %q/%q", cfg.Agents.Defaults.ModelName, cfg.Agents.Defaults.Provider)
		}
	})

	t.Run("empty api_key is dropped", func(t *testing.T) {
		path := writeRawConfigFile(t, `{"version":2,"model_list":[{"model_name":"m","model":"m","api_key":"   "}]}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if len(cfg.ModelList) != 1 || cfg.ModelList[0].APIKey() != "" {
			t.Fatalf("api key not dropped: %+v", cfg.ModelList[0])
		}
	})
}

func TestCoverageSaveConfigBackupReset(t *testing.T) {
	t.Run("save canonicalizes and hides virtual models", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		cfg := DefaultConfig()
		cfg.ModelList = []*ModelConfig{
			{ModelName: "m", Model: "m", APIKeys: SimpleSecureStrings("key-1")},
			{ModelName: "m__key_1", Model: "m", APIKeys: SimpleSecureStrings("key-2"), isVirtual: true},
		}
		cfg.Channels = ChannelsConfig{
			"pico": {Enabled: true, Type: "pico", Settings: mustParseRawNode(`{"token":"pico-tok"}`)},
		}
		original := cfg.ModelList
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatalf("SaveConfig() error = %v", err)
		}
		if len(cfg.ModelList) != len(original) {
			t.Fatalf("ModelList not restored after save: %d/%d", len(cfg.ModelList), len(original))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat saved config: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode = %o, want 600", got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved config: %v", err)
		}
		var top map[string]any
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatalf("saved config JSON: %v", err)
		}
		models, ok := top["model_list"].([]any)
		if !ok || len(models) != 1 {
			t.Fatalf("saved model_list = %v, want 1 non-virtual", top["model_list"])
		}
		entry := models[0].(map[string]any)
		if entry["api_key"] != "key-1" {
			t.Fatalf("saved api_key = %v", entry["api_key"])
		}
		if _, plural := entry["api_keys"]; plural {
			t.Fatal("saved entry still has api_keys")
		}
		channels, _ := top["channels"].(map[string]any)
		pico, _ := channels["pico"].(map[string]any)
		if pico["token"] != "pico-tok" {
			t.Fatalf("saved pico token = %v", pico["token"])
		}
		if top["version"] != float64(CurrentVersion) {
			t.Fatalf("saved version = %v", top["version"])
		}
	})

	t.Run("save bumps old version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		cfg := DefaultConfig()
		cfg.Version = 0
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatalf("SaveConfig() error = %v", err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(saved) error = %v", err)
		}
		if loaded.Version != CurrentVersion {
			t.Fatalf("version = %d", loaded.Version)
		}
	})

	t.Run("save marshal failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		cfg := DefaultConfig()
		cfg.ModelList = []*ModelConfig{{ModelName: "m", Model: "m", ExtraBody: map[string]any{"fn": func() {}}}}
		if err := SaveConfig(path, cfg); err == nil {
			t.Fatal("SaveConfig() expected marshal error")
		}
	})

	t.Run("make backup missing file", func(t *testing.T) {
		if err := MakeBackup(filepath.Join(t.TempDir(), "absent.json")); err != nil {
			t.Fatalf("MakeBackup(missing) error = %v", err)
		}
	})

	t.Run("make backup existing file and directory error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := MakeBackup(path); err != nil {
			t.Fatalf("MakeBackup() error = %v", err)
		}
		matches, err := filepath.Glob(path + ".*.bak")
		if err != nil || len(matches) != 1 {
			t.Fatalf("backup files = %v, %v", matches, err)
		}
		if err := MakeBackup(dir); err == nil {
			t.Fatal("MakeBackup(directory) expected copy error")
		}
	})

	t.Run("reset to defaults", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(`{"version":2,"bogus":1}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := ResetToDefaults(path); err != nil {
			t.Fatalf("ResetToDefaults() error = %v", err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(reset) error = %v", err)
		}
		if cfg.Version != CurrentVersion {
			t.Fatalf("reset version = %d", cfg.Version)
		}
		matches, _ := filepath.Glob(path + ".*.bak")
		if len(matches) != 1 {
			t.Fatalf("reset backup files = %v", matches)
		}
	})

	t.Run("canonicalize error and edge shapes", func(t *testing.T) {
		if _, err := canonicalizeSavedConfig([]byte("{"), DefaultConfig()); err == nil {
			t.Fatal("canonicalizeSavedConfig(invalid JSON) expected error")
		}
		cfg := DefaultConfig()
		cfg.ModelList = []*ModelConfig{{ModelName: "m", Model: "m"}}
		out, err := canonicalizeSavedConfig([]byte(`{"model_list":"not-a-list"}`), cfg)
		if err != nil {
			t.Fatalf("canonicalizeSavedConfig(non-list) error = %v", err)
		}
		if len(out) == 0 {
			t.Fatal("canonicalizeSavedConfig(non-list) empty output")
		}
		// Short model list / nil entries / empty api key branches.
		cfg.ModelList = []*ModelConfig{nil}
		if _, err := canonicalizeSavedConfig([]byte(`{"model_list":[{"api_keys":["x"]}]}`), cfg); err != nil {
			t.Fatalf("canonicalizeSavedConfig(nil model) error = %v", err)
		}
		// Break pico settings so GetDecoded fails: the channel is skipped.
		cfg.Channels = ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: RawNode(`{"streaming":"bad"}`)}}
		if _, err := canonicalizeSavedConfig([]byte(`{"channels":{}}`), cfg); err != nil {
			t.Fatalf("canonicalizeSavedConfig(bad pico settings) error = %v", err)
		}
	})
}

func TestCoverageWorkspacePathExpandHomeAndNameIndex(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no user home directory")
	}
	cfg := &Config{}
	cfg.Agents.Defaults.Workspace = ""
	if got := cfg.WorkspacePath(); got != "" {
		t.Fatalf("WorkspacePath(empty) = %q", got)
	}
	cfg.Agents.Defaults.Workspace = "~"
	if got := cfg.WorkspacePath(); got != home {
		t.Fatalf("WorkspacePath(~) = %q, want %q", got, home)
	}
	cfg.Agents.Defaults.Workspace = "~/ws"
	if got := cfg.WorkspacePath(); got != filepath.Join(home, "ws") {
		t.Fatalf("WorkspacePath(~/ws) = %q", got)
	}
	cfg.Agents.Defaults.Workspace = "~user/dir"
	if got := cfg.WorkspacePath(); got != home {
		t.Fatalf("WorkspacePath(~user/dir) = %q, want home", got)
	}
	cfg.Agents.Defaults.Workspace = "/tmp/plain"
	if got := cfg.WorkspacePath(); got != "/tmp/plain" {
		t.Fatalf("WorkspacePath(plain) = %q", got)
	}

	index := toNameIndex([]*ModelConfig{
		{ModelName: "m"},
		{ModelName: "m"},
		{ModelName: "other"},
	})
	want := []string{"m:0", "m:1", "other:0"}
	if len(index) != len(want) {
		t.Fatalf("toNameIndex = %v, want %v", index, want)
	}
	for i := range want {
		if index[i] != want[i] {
			t.Fatalf("toNameIndex[%d] = %q, want %q (%v)", i, index[i], want[i], index)
		}
	}
}

func TestCoverageLoaderHelpers(t *testing.T) {
	data := []byte(`{"agents":{"defaults":{"workspace":"/ws","max_tokens":10}},"tools":{"mcp":{"enabled":false}}}`)
	if !configObjectHasTopLevelField(data, "agents") {
		t.Fatal("configObjectHasTopLevelField(agents) = false")
	}
	if configObjectHasTopLevelField(data, "missing") {
		t.Fatal("configObjectHasTopLevelField(missing) = true")
	}
	if configObjectHasTopLevelField([]byte("{"), "agents") {
		t.Fatal("configObjectHasTopLevelField(invalid) = true")
	}
	if !configObjectHasField(data, "agents", "defaults") {
		t.Fatal("configObjectHasField(agents.defaults) = false")
	}
	if !configObjectHasField(data, "tools", "mcp") {
		t.Fatal("configObjectHasField(tools.mcp) = false")
	}
	if configObjectHasField(data, "agents", "workspace") {
		t.Fatal("configObjectHasField(agents.workspace) should only inspect the immediate object")
	}
	if configObjectHasField([]byte("{"), "agents", "defaults") {
		t.Fatal("configObjectHasField(invalid) = true")
	}
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"explicit false", `{"tools":{"mcp":{"enabled":false}}}`, true},
		{"explicit true", `{"tools":{"mcp":{"enabled":true}}}`, false},
		{"null", `{"tools":{"mcp":{"enabled":null}}}`, false},
		{"missing enabled", `{"tools":{"mcp":{"servers":{}}}}`, false},
		{"missing mcp", `{"tools":{}}`, false},
		{"missing tools", `{}`, false},
		{"invalid json", `{`, false},
		{"non-object tools", `{"tools":7}`, false},
		{"non-object mcp", `{"tools":{"mcp":7}}`, false},
		{"non-bool enabled", `{"tools":{"mcp":{"enabled":"no"}}}`, false},
	} {
		t.Run("mcp explicit disabled/"+tc.name, func(t *testing.T) {
			if got := rawMCPExplicitlyDisabled([]byte(tc.data)); got != tc.want {
				t.Fatalf("rawMCPExplicitlyDisabled(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestCoverageContextConfigs(t *testing.T) {
	t.Run("workspace apply defaults", func(t *testing.T) {
		var nilCfg *WorkspaceContextConfig
		nilCfg.ApplyDefaults()
		cfg := &WorkspaceContextConfig{}
		cfg.ApplyDefaults()
		if cfg.MaxBytes != DefaultWorkspaceContextMaxBytes ||
			cfg.MaxSourceBytes != DefaultWorkspaceContextMaxSourceBytes ||
			len(cfg.ProjectRootMarkers) == 0 || len(cfg.InstructionFiles) == 0 ||
			len(cfg.LocalInstructionFiles) == 0 {
			t.Fatalf("ApplyDefaults() = %+v", cfg)
		}
	})

	t.Run("workspace validate branches", func(t *testing.T) {
		var nilCfg *WorkspaceContextConfig
		if err := nilCfg.Validate(); err != nil {
			t.Fatalf("nil Validate() = %v", err)
		}
		valid := DefaultWorkspaceContextConfig()
		if err := valid.Validate(); err != nil {
			t.Fatalf("default Validate() = %v", err)
		}
		cases := []struct {
			name  string
			mut   func(*WorkspaceContextConfig)
			match string
		}{
			{"max_bytes", func(c *WorkspaceContextConfig) { c.MaxBytes = 0 }, "max_bytes must be > 0"},
			{"max_source_bytes", func(c *WorkspaceContextConfig) { c.MaxSourceBytes = 0 }, "max_source_bytes must be > 0"},
			{"empty candidate", func(c *WorkspaceContextConfig) { c.InstructionFiles = []string{"  "} }, "empty candidate name"},
			{"slash candidate", func(c *WorkspaceContextConfig) { c.ProjectRootMarkers = []string{"a/b"} }, "invalid candidate"},
			{"traversal candidate", func(c *WorkspaceContextConfig) { c.LocalInstructionFiles = []string{"../x"} }, "invalid candidate"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := DefaultWorkspaceContextConfig()
				tc.mut(&cfg)
				err := cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.match) {
					t.Fatalf("Validate() error = %v, want %q", err, tc.match)
				}
			})
		}
	})

	t.Run("runtime context apply/validate", func(t *testing.T) {
		var nilCfg *RuntimeContextConfig
		nilCfg.ApplyDefaults()
		if err := nilCfg.Validate(); err != nil {
			t.Fatalf("nil Validate() = %v", err)
		}
		cfg := &RuntimeContextConfig{}
		cfg.ApplyDefaults()
		if cfg.Mode != RuntimeContextModeSnapshot || cfg.RefreshIntervalSeconds != 600 {
			t.Fatalf("ApplyDefaults() = %+v", cfg)
		}
		for _, tc := range []struct {
			name  string
			cfg   RuntimeContextConfig
			match string
		}{
			{"invalid mode", RuntimeContextConfig{Mode: "sometimes"}, "runtime_context.mode"},
			{"negative interval", RuntimeContextConfig{Mode: "snapshot", RefreshIntervalSeconds: -1}, "refresh_interval_seconds must be >= 0"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.match) {
					t.Fatalf("Validate() error = %v, want %q", err, tc.match)
				}
			})
		}
		if err := (&RuntimeContextConfig{Mode: "system"}).Validate(); err != nil {
			t.Fatalf("system mode rejected: %v", err)
		}
	})

	t.Run("autonomy apply/validate", func(t *testing.T) {
		var nilCfg *AutonomyConfig
		nilCfg.ApplyDefaults()
		if err := nilCfg.Validate(); err != nil {
			t.Fatalf("nil Validate() = %v", err)
		}
		cfg := &AutonomyConfig{}
		cfg.ApplyDefaults()
		if cfg.MaxRounds != 20 || cfg.NoProgressRounds != 3 || cfg.RoundDelayMS != 1000 {
			t.Fatalf("ApplyDefaults() = %+v", cfg)
		}
		cases := []struct {
			name  string
			cfg   AutonomyConfig
			match string
		}{
			{"max_rounds low", AutonomyConfig{MaxRounds: 0, NoProgressRounds: 3}, "max_rounds must be between 1 and 200"},
			{"max_rounds high", AutonomyConfig{MaxRounds: 201, NoProgressRounds: 3}, "max_rounds must be between 1 and 200"},
			{"no_progress low", AutonomyConfig{MaxRounds: 20, NoProgressRounds: 0}, "no_progress_rounds must be between 1 and 10"},
			{"no_progress high", AutonomyConfig{MaxRounds: 20, NoProgressRounds: 11}, "no_progress_rounds must be between 1 and 10"},
			{"round delay", AutonomyConfig{MaxRounds: 20, NoProgressRounds: 3, RoundDelayMS: -1}, "round_delay_ms must be >= 0"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), tc.match) {
					t.Fatalf("Validate() error = %v, want %q", err, tc.match)
				}
			})
		}
	})

	t.Run("config context accessors and aggregate validation", func(t *testing.T) {
		var nilCfg *Config
		if nilCfg.WorkspaceContextEnabled() || nilCfg.AutonomyEnabled() {
			t.Fatal("nil config reported enabled context/autonomy")
		}
		if got := nilCfg.RuntimeContextMode(); got != RuntimeContextModeSnapshot {
			t.Fatalf("nil RuntimeContextMode() = %q", got)
		}
		if err := nilCfg.ValidateContextConfigs(); err != nil {
			t.Fatalf("nil ValidateContextConfigs() = %v", err)
		}

		cfg := DefaultConfig()
		if !cfg.WorkspaceContextEnabled() {
			t.Fatal("default workspace context should be enabled")
		}
		if !cfg.AutonomyEnabled() {
			t.Fatal("default autonomy should be enabled")
		}
		cfg.Agents.Defaults.RuntimeContext.Mode = "   "
		if got := cfg.RuntimeContextMode(); got != RuntimeContextModeSnapshot {
			t.Fatalf("blank RuntimeContextMode() = %q", got)
		}
		cfg.Agents.Defaults.RuntimeContext.Mode = "SYSTEM"
		if got := cfg.RuntimeContextMode(); got != "system" {
			t.Fatalf("RuntimeContextMode() = %q", got)
		}
		cfg.ApplyContextDefaults()
		if err := cfg.ValidateContextConfigs(); err != nil {
			t.Fatalf("ValidateContextConfigs(default) = %v", err)
		}
		cfg.Agents.Defaults.WorkspaceContext.MaxBytes = 0
		if err := cfg.ValidateContextConfigs(); err == nil {
			t.Fatal("ValidateContextConfigs() expected workspace error")
		}
	})
}

func TestCoverageExplicitContextZeroBranches(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"invalid json ignored", `{`, ""},
		{"valid omitted", `{}`, ""},
		{"max_bytes zero", `{"agents":{"defaults":{"workspace_context":{"max_bytes":0}}}}`, "workspace_context.max_bytes"},
		{"max_source_bytes zero", `{"agents":{"defaults":{"workspace_context":{"max_source_bytes":0}}}}`, "workspace_context.max_source_bytes"},
		{"max_rounds zero", `{"agents":{"defaults":{"autonomy":{"max_rounds":0}}}}`, "autonomy.max_rounds"},
		{"no_progress_rounds zero", `{"agents":{"defaults":{"autonomy":{"no_progress_rounds":0}}}}`, "autonomy.no_progress_rounds"},
		{"null omitted", `{"agents":{"defaults":{"autonomy":{"max_rounds":null}}}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExplicitContextZeros([]byte(tc.raw))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateExplicitContextZeros() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateExplicitContextZeros() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCoverageEnvAndEvents(t *testing.T) {
	t.Setenv(EnvWorkspace, "/tmp/ws-env")
	if got := GetWorkspace(); got != "/tmp/ws-env" {
		t.Fatalf("GetWorkspace() = %q", got)
	}
	t.Setenv(EnvWorkspace, "")
	if got := GetWorkspace(); got != "" {
		t.Fatalf("GetWorkspace(empty) = %q", got)
	}

	t.Setenv(EnvHome, "/tmp/pico-home")
	if got := GetHome(); got != "/tmp/pico-home" {
		t.Fatalf("GetHome(env) = %q", got)
	}
	t.Setenv(EnvHome, "")
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if got := GetHome(); got != filepath.Join(home, pkg.DefaultChagentHome) {
			t.Fatalf("GetHome() = %q, want home-based", got)
		}
	}

	defaults := EffectiveEventLoggingConfig(nil)
	if !defaults.Enabled || len(defaults.Include) != 1 || defaults.Include[0] != "agent.*" || defaults.MinSeverity != "info" {
		t.Fatalf("EffectiveEventLoggingConfig(nil) = %+v", defaults)
	}
	cfg := &Config{}
	got := EffectiveEventLoggingConfig(cfg)
	if got.Enabled {
		t.Fatalf("zero config logging enabled = %+v", got)
	}
	if got.MinSeverity != "info" || len(got.Include) != 1 {
		t.Fatalf("zero config defaults = %+v", got)
	}
	cfg.Events.Logging = EventLoggingConfig{
		Enabled:     true,
		Include:     []string{"custom.*"},
		MinSeverity: "debug",
	}
	got = EffectiveEventLoggingConfig(cfg)
	if !got.Enabled || got.Include[0] != "custom.*" || got.MinSeverity != "debug" {
		t.Fatalf("custom logging = %+v", got)
	}
}

func TestCoverageAgentNameHelpers(t *testing.T) {
	if got := ResolveAgentName(""); got != "default" {
		t.Fatalf("ResolveAgentName(empty) = %q", got)
	}
	if got := ResolveAgentName("  named  "); got != "named" {
		t.Fatalf("ResolveAgentName(spaces) = %q", got)
	}
	token := DerivePicoToken("agent")
	if len(token) != 32 {
		t.Fatalf("DerivePicoToken len = %d", len(token))
	}
	if DerivePicoToken("agent") != token {
		t.Fatal("DerivePicoToken not deterministic")
	}
	if err := ApplyNameDerivedPicoToken(nil, "x"); err != nil {
		t.Fatalf("ApplyNameDerivedPicoToken(nil) = %v", err)
	}

	cfg := &Config{}
	if err := ApplyNameDerivedPicoToken(cfg, "agent"); err != nil {
		t.Fatalf("ApplyNameDerivedPicoToken() error = %v", err)
	}
	pico := cfg.Channels.Get("pico")
	if pico == nil || !pico.Enabled || pico.Type != "pico" {
		t.Fatalf("pico channel = %+v", pico)
	}
	decoded, err := pico.GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded() error = %v", err)
	}
	if settings, ok := decoded.(*PicoSettings); !ok || settings.Token.String() != token {
		t.Fatalf("pico token = %+v", decoded)
	}

	// Existing pico channel keeps its name/type but gets the derived token.
	cfg = &Config{Channels: ChannelsConfig{"pico": {}}}
	if err := ApplyNameDerivedPicoToken(cfg, "agent"); err != nil {
		t.Fatalf("ApplyNameDerivedPicoToken(existing) error = %v", err)
	}
	if !cfg.Channels["pico"].Enabled || cfg.Channels["pico"].Type != "pico" {
		t.Fatalf("existing pico channel not activated: %+v", cfg.Channels["pico"])
	}

	// Decode failure is surfaced.
	bad := &Config{Channels: ChannelsConfig{"pico": {Enabled: true, Type: "pico", Settings: RawNode(`{"streaming":"bad"}`)}}}
	if err := ApplyNameDerivedPicoToken(bad, "agent"); err == nil {
		t.Fatal("ApplyNameDerivedPicoToken(bad settings) expected error")
	}
}

func TestCoverageDiagnosticsHelpers(t *testing.T) {
	if got := DiagnosticSummary(nil); got != "" {
		t.Fatalf("DiagnosticSummary(nil) = %q", got)
	}
	if got := DiagnosticSummary(errors.New("summary\npreview")); got != "summary" {
		t.Fatalf("DiagnosticSummary() = %q", got)
	}
	if got := DiagnosticSummary(errors.New("\x1b[31mred\x1b[0m error")); got != "red error" {
		t.Fatalf("DiagnosticSummary(ANSI) = %q", got)
	}

	if got := formatDiagnosticLogMessage("prefix", nil); got != "prefix" {
		t.Fatalf("formatDiagnosticLogMessage(nil) = %q", got)
	}
	if got := formatDiagnosticLogMessage("prefix", errors.New("only-summary")); got != "prefix: only-summary" {
		t.Fatalf("formatDiagnosticLogMessage(summary) = %q", got)
	}
	if got := formatDiagnosticLogMessage("prefix", errors.New("\npreview-only")); !strings.Contains(got, "prefix") || !strings.Contains(got, "preview-only") {
		t.Fatalf("formatDiagnosticLogMessage(preview) = %q", got)
	}
	if got := formatDiagnosticLogMessage("prefix", errors.New("")); got != "prefix" {
		t.Fatalf("formatDiagnosticLogMessage(empty) = %q", got)
	}

	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[not-a-sequence", "ot-a-sequence"},
		{"a\x1bb", "ab"},
	} {
		if got := stripANSISequences(tc.in); got != tc.want {
			t.Fatalf("stripANSISequences(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// wrapJSONError branches: syntax, type, default.
	syntaxErr := wrapJSONError([]byte(`{"a":}`), mustJSONSyntaxError(t, `{"a":}`), "config.json")
	if !strings.Contains(syntaxErr.Error(), "syntax error") {
		t.Fatalf("wrapJSONError(syntax) = %v", syntaxErr)
	}
	typeErr := wrapJSONError([]byte(`{"n":"x"}`), mustJSONTypeError(t, `{"n":"x"}`), "config.json")
	if !strings.Contains(typeErr.Error(), "type error") {
		t.Fatalf("wrapJSONError(type) = %v", typeErr)
	}
	other := wrapJSONError(nil, errors.New("boom"), "config.json")
	if !strings.Contains(other.Error(), "failed to parse config.json") {
		t.Fatalf("wrapJSONError(other) = %v", other)
	}

	// Preview/line/trim helpers.
	if got := diagnosticPreviewForOffset(nil, 0); got != "" {
		t.Fatalf("diagnosticPreviewForOffset(nil) = %q", got)
	}
	if got := diagnosticPreviewForOffset([]byte("a\n\nb\n"), 3); got != "" {
		t.Fatalf("diagnosticPreviewForOffset(blank line) = %q", got)
	}
	if got := diagnosticPreviewForOffset([]byte("abc\ndef\n"), 1); !strings.Contains(got, "abc") {
		t.Fatalf("diagnosticPreviewForOffset(abc) = %q", got)
	}
	if start, end := lineBoundsForOffset(nil, 0); start != 0 || end != 0 {
		t.Fatalf("lineBoundsForOffset(nil) = %d,%d", start, end)
	}
	if start, end := lineBoundsForOffset([]byte("abc\ndef"), -5); start != 0 || end != 3 {
		t.Fatalf("lineBoundsForOffset(neg) = %d,%d", start, end)
	}
	if start, end := lineBoundsForOffset([]byte("abc\ndef"), 100); start != 4 || end != 7 {
		t.Fatalf("lineBoundsForOffset(huge) = %d,%d", start, end)
	}
	if line, column := lineAndColumnForOffset([]byte("abc\ndef"), 0); line != 1 || column != 1 {
		t.Fatalf("lineAndColumnForOffset(0) = %d,%d", line, column)
	}
	if line, column := lineAndColumnForOffset([]byte("abc\ndef"), 6); line != 2 || column != 2 {
		t.Fatalf("lineAndColumnForOffset(6) = %d,%d", line, column)
	}
	if line, column := lineAndColumnForOffset([]byte("abc\ndef"), 5); line != 2 || column != 1 {
		t.Fatalf("lineAndColumnForOffset(5) = %d,%d", line, column)
	}
	if line, column := lineAndColumnForOffset([]byte("abc"), 100); line != 1 || column != 3 {
		t.Fatalf("lineAndColumnForOffset(100) = %d,%d", line, column)
	}
	if trimmed, offset := trimDiagnosticLine("short", 1); trimmed != "short" || offset != 0 {
		t.Fatalf("trimDiagnosticLine(short) = %q,%d", trimmed, offset)
	}
	if trimmed, offset := trimDiagnosticLine("", 1); trimmed != "" || offset != 0 {
		t.Fatalf("trimDiagnosticLine(empty) = %q,%d", trimmed, offset)
	}
	long := strings.Repeat("x", 400)
	if trimmed, offset := trimDiagnosticLine(long, 200); !strings.HasPrefix(trimmed, "...") || !strings.HasSuffix(trimmed, "...") {
		t.Fatalf("trimDiagnosticLine(long middle) = %q,%d", trimmed, offset)
	}
	if trimmed, offset := trimDiagnosticLine(long, 1); strings.HasPrefix(trimmed, "...") || offset != 0 {
		t.Fatalf("trimDiagnosticLine(long start) = %q,%d", trimmed, offset)
	}
	if trimmed, offset := trimDiagnosticLine(long, 399); !strings.HasPrefix(trimmed, "...") || offset != 237 {
		t.Fatalf("trimDiagnosticLine(long end) = %q,%d", trimmed, offset)
	}
	if got := maxRuneCount("abc", 0); got != 0 {
		t.Fatalf("maxRuneCount(0) = %d", got)
	}
	if got := maxRuneCount("abc", 10); got != 3 {
		t.Fatalf("maxRuneCount(10) = %d", got)
	}
	if got := highlightDiagnosticColumn("abc", 0); got != "abc" {
		t.Fatalf("highlightDiagnosticColumn(0) = %q", got)
	}
	if got := highlightDiagnosticColumn("abc", 9); got != "abc" {
		t.Fatalf("highlightDiagnosticColumn(9) = %q", got)
	}
	if got := highlightDiagnosticColumn("abc", 2); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("highlightDiagnosticColumn(2) = %q", got)
	}

	// Unknown-field collection: nested struct, slice and map paths, underscore skip.
	type inner struct {
		Known int `json:"known"`
	}
	type outer struct {
		Inner  inner            `json:"inner"`
		Items  []inner          `json:"items"`
		Lookup map[string]inner `json:"lookup"`
		Skip   string           `json:"-"`
	}
	raw := map[string]any{
		"inner":  map[string]any{"known": 1, "unknown": 2},
		"items":  []any{map[string]any{"known": 1, "item_bad": 3}},
		"lookup": map[string]any{"k": map[string]any{"map_bad": 4}},
		"_note":  "ignored",
		"top":    "bad",
	}
	issues := collectUnknownJSONFields(raw, reflect.TypeOf(outer{}), "")
	joined := strings.Join(issues, ",")
	for _, want := range []string{"inner.unknown", "items[0].item_bad", "lookup.k.map_bad", "top"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("collectUnknownJSONFields = %v, missing %q", issues, want)
		}
	}
	if strings.Contains(joined, "_note") {
		t.Fatalf("underscore field reported: %v", issues)
	}
	if got := collectUnknownJSONFields(raw, nil, "x"); got != nil {
		t.Fatalf("collectUnknownJSONFields(nil type) = %v", got)
	}
	if got := collectUnknownJSONFields("scalar", reflect.TypeOf(outer{}), ""); got != nil {
		t.Fatalf("collectUnknownJSONFields(scalar) = %v", got)
	}
}

func mustJSONSyntaxError(t *testing.T, data string) error {
	t.Helper()
	var v any
	err := json.Unmarshal([]byte(data), &v)
	if err == nil {
		t.Fatalf("json.Unmarshal(%q) unexpectedly succeeded", data)
	}
	return err
}

func mustJSONTypeError(t *testing.T, data string) error {
	t.Helper()
	var v struct {
		N int `json:"n"`
	}
	err := json.Unmarshal([]byte(data), &v)
	if err == nil {
		t.Fatalf("json.Unmarshal(%q) unexpectedly succeeded", data)
	}
	return err
}
