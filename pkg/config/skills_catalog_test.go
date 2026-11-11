// Chagent - 超轻量级个人 AI agent

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalSkillsCatalogConfig 是规范 v2 配置，用于检验
// tools.skills.catalog_mode 开关（T4.3）。
func minimalSkillsCatalogConfig(extraTools string) string {
	return `{
  "version": 2,
  "model_list": [
    {
      "model_name": "glm-catalog",
      "model": "glm-5.3",
      "provider": "zhipu",
      "api_base": "https://open.bigmodel.cn/api/paas/v4",
      "api_key": "test-key"
    }
  ],
  "tools": {
    "skills": {
      "enabled": true` + extraTools + `
    }
  }
}`
}

func writeSkillsCatalogConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestSkillsCatalogModeDefaults(t *testing.T) {
	cfg := DefaultConfig()
	// DefaultConfig 不运行 ApplyDefaults；规范化必须已经把零值视为默认 "context" 模式。
	if got := cfg.Tools.Skills.EffectiveCatalogMode(); got != SkillsCatalogModeContext {
		t.Fatalf("EffectiveCatalogMode() = %q, want %q", got, SkillsCatalogModeContext)
	}

	cfg.Tools.Skills.ApplyDefaults()
	if got := cfg.Tools.Skills.CatalogMode; got != SkillsCatalogModeContext {
		t.Fatalf("CatalogMode after ApplyDefaults() = %q, want %q", got, SkillsCatalogModeContext)
	}
	if err := cfg.Tools.Skills.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestSkillsCatalogModeExplicitSystemPreserved(t *testing.T) {
	cfg := SkillsToolsConfig{CatalogMode: SkillsCatalogModeSystem}
	cfg.ApplyDefaults()
	if cfg.CatalogMode != SkillsCatalogModeSystem {
		t.Fatalf("ApplyDefaults() overwrote explicit mode: %q", cfg.CatalogMode)
	}
	if got := cfg.EffectiveCatalogMode(); got != SkillsCatalogModeSystem {
		t.Fatalf("EffectiveCatalogMode() = %q, want system", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	// 未知模式在校验时被拒绝，但规范化时仍防御性处理。
	unknown := SkillsToolsConfig{CatalogMode: "bogus"}
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "catalog_mode") {
		t.Fatalf("Validate(bogus) error = %v, want catalog_mode error", err)
	}
	if got := unknown.EffectiveCatalogMode(); got != SkillsCatalogModeContext {
		t.Fatalf("EffectiveCatalogMode(bogus) = %q, want context fallback", got)
	}
}

func TestLoadConfigSkillsCatalogMode(t *testing.T) {
	t.Run("omitted defaults to context", func(t *testing.T) {
		path := writeSkillsCatalogConfig(t, minimalSkillsCatalogConfig(""))
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if got := cfg.Tools.Skills.CatalogMode; got != SkillsCatalogModeContext {
			t.Fatalf("CatalogMode = %q, want context", got)
		}
	})

	t.Run("explicit system is preserved", func(t *testing.T) {
		path := writeSkillsCatalogConfig(t, minimalSkillsCatalogConfig(`,
      "catalog_mode": "system"`))
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if got := cfg.Tools.Skills.CatalogMode; got != SkillsCatalogModeSystem {
			t.Fatalf("CatalogMode = %q, want system", got)
		}
	})

	t.Run("invalid mode is rejected", func(t *testing.T) {
		path := writeSkillsCatalogConfig(t, minimalSkillsCatalogConfig(`,
      "catalog_mode": "sometimes"`))
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() with invalid catalog_mode should fail")
		}
		if !strings.Contains(err.Error(), "tools.skills.catalog_mode") {
			t.Fatalf("LoadConfig() error = %v, want catalog_mode validation error", err)
		}
	})
}
