package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// chessV2Config is the minimal shape chess/client writes: model_list with the
// singular api_key form, channels.pico.token, and an optional gateway block.
const chessV2Config = `{
  "version": 2,
  "model_list": [
    {
      "model_name": "glm-chess",
      "model": "glm-5.3",
      "provider": "zhipu",
      "api_base": "https://open.bigmodel.cn/api/paas/v4",
      "api_key": "test-key"
    }
  ],
  "gateway": {
    "host": "127.0.0.1",
    "port": 0
  }
}`

func writeChessConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvWorkspace, "")
	t.Setenv(AgentNameEnv, "chess-agent")
	return path
}

func TestChessMinimalConfigLoads(t *testing.T) {
	path := writeChessConfig(t, chessV2Config)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Agents.Defaults.ModelName != "glm-chess" {
		t.Fatalf("ModelName = %q, want inferred glm-chess", cfg.Agents.Defaults.ModelName)
	}
	if cfg.Agents.Defaults.Provider != "zhipu" {
		t.Fatalf("Provider = %q, want zhipu", cfg.Agents.Defaults.Provider)
	}
	if got := cfg.ModelList[0].APIKey(); got != "test-key" {
		t.Fatalf("APIKey() = %q, want singular api_key normalized", got)
	}
	if cfg.Agents.Defaults.Workspace != filepath.Dir(path) {
		t.Fatalf("Workspace = %q, want config dir %q", cfg.Agents.Defaults.Workspace, filepath.Dir(path))
	}
	if cfg.Agents.Defaults.Autonomy.Enabled != true {
		t.Fatal("autonomy should be enabled by chess default")
	}
	if cfg.Agents.Defaults.WorkspaceContext.UserGlobal {
		t.Fatal("workspace_context.user_global should default false for chess")
	}
	if cfg.Gateway.Host != "127.0.0.1" || cfg.Gateway.Port != 0 {
		t.Fatalf("Gateway = %s:%d, want 127.0.0.1:0", cfg.Gateway.Host, cfg.Gateway.Port)
	}

	pico := cfg.Channels.Get("pico")
	if pico == nil {
		t.Fatal("pico channel missing after v2 load")
	}
	decoded, decodeErr := pico.GetDecoded()
	if decodeErr != nil {
		t.Fatalf("pico GetDecoded: %v", decodeErr)
	}
	settings, ok := decoded.(*PicoSettings)
	if !ok {
		t.Fatalf("pico settings type = %T", decoded)
	}
	wantToken := DerivePicoToken("chess-agent")
	if settings.Token.String() != wantToken {
		t.Fatalf("pico token = %q, want md5(name) %q", settings.Token.String(), wantToken)
	}

	// config.json contains no pico token: the md5(name) convention is external.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if _, ok := top["channels"]; ok {
		t.Fatalf("auto pico channel should not be written to config: %s", raw)
	}
}

func TestPicoChannelAutoEnabledWithoutConfig(t *testing.T) {
	path := writeChessConfig(t, `{"version":2,"model_list":[{"model_name":"m","model":"m","api_key":"k"}]}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	pico := cfg.Channels.Get("pico")
	if pico == nil || !pico.Enabled {
		t.Fatal("pico channel should be auto-enabled without channels config")
	}
}
