package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, raw string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	return path
}

// 未出现 execution_report 时默认开启。
func TestExecutionReportConfigDefaultEnabled(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, `{"version":2}`))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !cfg.GetExecutionReport().IsEnabled() {
		t.Fatal("execution_report must default to enabled")
	}
	if !DefaultConfig().GetExecutionReport().IsEnabled() {
		t.Fatal("DefaultConfig() must keep execution_report enabled")
	}
}

// 显式 false 是唯一的关闭方式。
func TestExecutionReportConfigExplicitFalse(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, `{"version":2,"execution_report":{"enabled":false}}`))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.GetExecutionReport().IsEnabled() {
		t.Fatal("explicit execution_report.enabled=false must disable reports")
	}
}

func TestExecutionReportConfigExplicitTrueAndEmpty(t *testing.T) {
	cases := []string{
		`{"version":2,"execution_report":{"enabled":true}}`,
		`{"version":2,"execution_report":{}}`,
	}
	for _, raw := range cases {
		cfg, err := LoadConfig(writeConfigFile(t, raw))
		if err != nil {
			t.Fatalf("LoadConfig(%s) error = %v", raw, err)
		}
		if !cfg.GetExecutionReport().IsEnabled() {
			t.Fatalf("%s must keep execution_report enabled", raw)
		}
	}
}

// 该键必须被 strict 解码接受（不能成为 unknown field）。
func TestExecutionReportConfigIsKnownField(t *testing.T) {
	if _, err := LoadConfig(writeConfigFile(t, `{"version":2,"execution_report":{"enabled":true}}`)); err != nil {
		t.Fatalf("execution_report rejected as unknown field: %v", err)
	}
}

// 子字段仍受 strict 校验保护。
func TestExecutionReportConfigRejectsUnknownSubfield(t *testing.T) {
	_, err := LoadConfig(writeConfigFile(t, `{"version":2,"execution_report":{"enable":true}}`))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "execution_report.enable") {
		t.Fatalf("error should name execution_report.enable, got %q", err.Error())
	}
}

func TestExecutionReportConfigNilReceiverIsSafe(t *testing.T) {
	var cfg *Config
	if !cfg.GetExecutionReport().IsEnabled() {
		t.Fatal("nil Config must be treated as enabled")
	}
	// 零值（未显式设置）同样视为开启。
	if !(ExecutionReportConfig{}).IsEnabled() {
		t.Fatal("zero ExecutionReportConfig must be treated as enabled")
	}
	// SetEnabled 对 nil 接收者不得 panic。
	var report *ExecutionReportConfig
	report.SetEnabled(false)
}
