package config

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/netbind"
)

const DefaultGatewayLogLevel = "warn"

type GatewayConfig struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	HotReload bool   `json:"hot_reload"`
	LogLevel  string `json:"log_level,omitempty"`
}

func canonicalGatewayLogLevel(level logger.LogLevel) string {
	switch level {
	case logger.DEBUG:
		return "debug"
	case logger.INFO:
		return "info"
	case logger.WARN:
		return "warn"
	case logger.ERROR:
		return "error"
	case logger.FATAL:
		return "fatal"
	default:
		return DefaultGatewayLogLevel
	}
}

func normalizeGatewayLogLevel(logLevel string) string {
	if level, ok := logger.ParseLevel(logLevel); ok {
		return canonicalGatewayLogLevel(level)
	}
	return DefaultGatewayLogLevel
}

// EffectiveGatewayLogLevel 返回已加载配置中规范化后的运行时日志级别。
// 无效或空值会回退到本包的默认值。
func EffectiveGatewayLogLevel(cfg *Config) string {
	if cfg == nil {
		return DefaultGatewayLogLevel
	}
	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}

func normalizeGatewayHostInput(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(DefaultConfig().Gateway.Host)
	}
	if host == "" {
		host = "localhost"
	}
	return netbind.NormalizeHostInput(host)
}

// ResolveGatewayLogLevel 读取已配置的 gateway 日志级别，而不触发完整的
// 配置加载器，这样启动代码就能在配置加载日志输出之前先应用日志设置。
// CHAGENT_LOG_LEVEL 环境变量会覆盖文件中的取值。
func ResolveGatewayLogLevel(path string) string {
	cfg := struct {
		Gateway GatewayConfig `json:"gateway"`
	}{
		Gateway: GatewayConfig{LogLevel: DefaultGatewayLogLevel},
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			logger.WarnCF("config", "failed to parse gateway config, using defaults", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
		}
	}

	if envLevel := os.Getenv("CHAGENT_LOG_LEVEL"); envLevel != "" {
		cfg.Gateway.LogLevel = envLevel
	}

	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}
