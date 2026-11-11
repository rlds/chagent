package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContextZerosConfig 写入一份最小可加载的 v2 配置。agentsDefaults 是
// agents.defaults 对象的完整 JSON（可为空表示完全省略该对象）。
func writeContextZerosConfig(t *testing.T, agentsDefaults string) string {
	t.Helper()

	body := `{"version":2,"model_list":[{"model_name":"m","model":"m","api_key":"k"}]`
	if strings.TrimSpace(agentsDefaults) != "" {
		body += `,"agents":{"defaults":` + agentsDefaults + `}`
	}
	body += `}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvConfig, path)
	t.Setenv(EnvWorkspace, "")
	t.Setenv(AgentNameEnv, "context-zeros-agent")
	return path
}

func TestLoadConfigRejectsExplicitNonPositiveContextValues(t *testing.T) {
	tests := []struct {
		name     string
		defaults string
		wantErr  string
	}{
		{
			name:     "autonomy max_rounds explicit zero",
			defaults: `{"autonomy":{"enabled":true,"max_rounds":0}}`,
			wantErr:  "agents.defaults.autonomy.max_rounds must be between 1 and 200, got 0",
		},
		{
			name:     "autonomy max_rounds negative",
			defaults: `{"autonomy":{"enabled":true,"max_rounds":-5}}`,
			wantErr:  "agents.defaults.autonomy.max_rounds must be between 1 and 200, got -5",
		},
		{
			name:     "autonomy no_progress_rounds explicit zero",
			defaults: `{"autonomy":{"enabled":true,"no_progress_rounds":0}}`,
			wantErr:  "agents.defaults.autonomy.no_progress_rounds must be between 1 and 10, got 0",
		},
		{
			name:     "autonomy no_progress_rounds negative",
			defaults: `{"autonomy":{"enabled":true,"no_progress_rounds":-2}}`,
			wantErr:  "agents.defaults.autonomy.no_progress_rounds must be between 1 and 10, got -2",
		},
		{
			name:     "workspace_context max_bytes explicit zero",
			defaults: `{"workspace_context":{"enabled":true,"max_bytes":0}}`,
			wantErr:  "agents.defaults.workspace_context.max_bytes must be > 0, got 0",
		},
		{
			name:     "workspace_context max_bytes negative",
			defaults: `{"workspace_context":{"enabled":true,"max_bytes":-1}}`,
			wantErr:  "agents.defaults.workspace_context.max_bytes must be > 0, got -1",
		},
		{
			name:     "workspace_context max_source_bytes explicit zero",
			defaults: `{"workspace_context":{"enabled":true,"max_source_bytes":0}}`,
			wantErr:  "agents.defaults.workspace_context.max_source_bytes must be > 0, got 0",
		},
		{
			name:     "workspace_context max_source_bytes negative",
			defaults: `{"workspace_context":{"enabled":true,"max_source_bytes":-7}}`,
			wantErr:  "agents.defaults.workspace_context.max_source_bytes must be > 0, got -7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeContextZerosConfig(t, tt.defaults)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig() accepted an explicitly non-positive value: %s", tt.defaults)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadConfig() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadConfigAppliesContextDefaultsWhenKeysOmitted(t *testing.T) {
	path := writeContextZerosConfig(t, "")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	autonomy := cfg.Agents.Defaults.Autonomy
	if autonomy.MaxRounds != 20 {
		t.Fatalf("autonomy.max_rounds = %d, want default 20", autonomy.MaxRounds)
	}
	if autonomy.NoProgressRounds != 3 {
		t.Fatalf("autonomy.no_progress_rounds = %d, want default 3", autonomy.NoProgressRounds)
	}
	workspace := cfg.Agents.Defaults.WorkspaceContext
	if workspace.MaxBytes != DefaultWorkspaceContextMaxBytes {
		t.Fatalf("workspace_context.max_bytes = %d, want default %d",
			workspace.MaxBytes, DefaultWorkspaceContextMaxBytes)
	}
	if workspace.MaxSourceBytes != DefaultWorkspaceContextMaxSourceBytes {
		t.Fatalf("workspace_context.max_source_bytes = %d, want default %d",
			workspace.MaxSourceBytes, DefaultWorkspaceContextMaxSourceBytes)
	}
}

func TestLoadConfigKeepsExplicitPositiveContextValues(t *testing.T) {
	path := writeContextZerosConfig(
		t,
		`{"autonomy":{"enabled":true,"max_rounds":7,"no_progress_rounds":2},`+
			`"workspace_context":{"enabled":true,"max_bytes":4096,"max_source_bytes":8192}}`,
	)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	autonomy := cfg.Agents.Defaults.Autonomy
	if autonomy.MaxRounds != 7 || autonomy.NoProgressRounds != 2 {
		t.Fatalf("autonomy = {max_rounds:%d no_progress_rounds:%d}, want {7 2}",
			autonomy.MaxRounds, autonomy.NoProgressRounds)
	}
	workspace := cfg.Agents.Defaults.WorkspaceContext
	if workspace.MaxBytes != 4096 || workspace.MaxSourceBytes != 8192 {
		t.Fatalf("workspace_context = {max_bytes:%d max_source_bytes:%d}, want {4096 8192}",
			workspace.MaxBytes, workspace.MaxSourceBytes)
	}
}

// round_delay_ms = 0 is legal: it must not be rejected even though the other
// numeric autonomy knobs reject explicit non-positive values.
func TestLoadConfigAllowsExplicitZeroRoundDelay(t *testing.T) {
	path := writeContextZerosConfig(t, `{"autonomy":{"enabled":true,"round_delay_ms":0}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig() rejected round_delay_ms=0: %v", err)
	}
}

// Explicit null is treated as "absent" (it decodes like an omitted key), so the
// default is still applied instead of failing the load.
func TestLoadConfigTreatsNullContextValuesAsOmitted(t *testing.T) {
	path := writeContextZerosConfig(
		t,
		`{"autonomy":{"enabled":true,"max_rounds":null,"no_progress_rounds":null},`+
			`"workspace_context":{"enabled":true,"max_bytes":null,"max_source_bytes":null}}`,
	)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	autonomy := cfg.Agents.Defaults.Autonomy
	if autonomy.MaxRounds != 20 || autonomy.NoProgressRounds != 3 {
		t.Fatalf("autonomy = {%d %d}, want defaults {20 3}", autonomy.MaxRounds, autonomy.NoProgressRounds)
	}
}
