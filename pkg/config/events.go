package config

// EventsConfig 汇总运行时事件配置。
type EventsConfig struct {
	Logging EventLoggingConfig `json:"logging,omitempty"`
}

// EventLoggingConfig 控制集中式运行时事件日志记录。
type EventLoggingConfig struct {
	// Enabled 控制内置日志器是否打印运行时事件。
	Enabled bool `json:"enabled"`
	// Include 包含精确的事件类型或通配模式，例如 "agent.*" 或 "*"。
	Include []string `json:"include,omitempty"`
	// Exclude 包含在 Include 匹配之后需要抑制的精确事件类型或通配模式。
	Exclude []string `json:"exclude,omitempty"`
	// MinSeverity 过滤掉低于所配置严重级别的事件：debug、info、warn 或 error。
	MinSeverity string `json:"min_severity,omitempty"`
	// IncludePayload 将原始 payload 加入日志。除非需要详细诊断，否则应保持禁用。
	IncludePayload bool `json:"include_payload,omitempty"`
}

// DefaultEventLoggingInclude 保留既有行为：agent 事件仍会被打印，
// 而非 agent 的运行时事件仅发布给订阅者。
var DefaultEventLoggingInclude = []string{"agent.*"}

// EffectiveEventLoggingConfig 返回带有稳定默认值的日志配置。
func EffectiveEventLoggingConfig(cfg *Config) EventLoggingConfig {
	if cfg == nil {
		return defaultEventLoggingConfig()
	}

	out := cfg.Events.Logging
	if out.MinSeverity == "" {
		out.MinSeverity = "info"
	}
	if len(out.Include) == 0 {
		out.Include = append([]string(nil), DefaultEventLoggingInclude...)
	}
	return out
}

func defaultEventLoggingConfig() EventLoggingConfig {
	return EventLoggingConfig{
		Enabled:     true,
		Include:     append([]string(nil), DefaultEventLoggingInclude...),
		MinSeverity: "info",
	}
}
