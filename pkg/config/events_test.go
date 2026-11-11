package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultEventLoggingConfig(t *testing.T) {
	cfg := DefaultConfig()
	logCfg := EffectiveEventLoggingConfig(cfg)

	if !logCfg.Enabled {
		t.Fatal("default event logging should be enabled")
	}
	if !reflect.DeepEqual(logCfg.Include, []string{"agent.*"}) {
		t.Fatalf("default include = %#v, want agent.*", logCfg.Include)
	}
	if logCfg.MinSeverity != "info" {
		t.Fatalf("default min severity = %q, want info", logCfg.MinSeverity)
	}
	if logCfg.IncludePayload {
		t.Fatal("default event logging should not include raw payloads")
	}
}

func TestLoadConfigEventLoggingOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"version": 2,
		"events": {
			"logging": {
				"enabled": false,
				"include": ["gateway.*"],
				"exclude": ["gateway.ready"],
				"min_severity": "warn",
				"include_payload": true
			}
		}
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	logCfg := EffectiveEventLoggingConfig(cfg)

	if logCfg.Enabled {
		t.Fatal("loaded event logging enabled = true, want false")
	}
	if !reflect.DeepEqual(logCfg.Include, []string{"gateway.*"}) {
		t.Fatalf("loaded include = %#v, want gateway.*", logCfg.Include)
	}
	if !reflect.DeepEqual(logCfg.Exclude, []string{"gateway.ready"}) {
		t.Fatalf("loaded exclude = %#v, want gateway.ready", logCfg.Exclude)
	}
	if logCfg.MinSeverity != "warn" {
		t.Fatalf("loaded min severity = %q, want warn", logCfg.MinSeverity)
	}
	if !logCfg.IncludePayload {
		t.Fatal("loaded include_payload = false, want true")
	}
}
