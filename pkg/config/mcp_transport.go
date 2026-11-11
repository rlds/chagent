package config

import "strings"

// NormalizeMCPTransportType 规范化配置中使用的 MCP 传输名称。
// "http" 是 Chagent 的可流式 HTTP 请求-响应模式，
// "streamable-http" 作为同一传输的显式别名被接受。
func NormalizeMCPTransportType(transport string) string {
	normalized := strings.ToLower(strings.TrimSpace(transport))

	switch normalized {
	case "streamable-http", "streamable_http", "streamablehttp":
		return "http"
	default:
		return normalized
	}
}

// EffectiveMCPTransportType 返回规范化后的已配置传输类型；
// 当配置中 Type 为空时，返回推断出的默认值。
func EffectiveMCPTransportType(server MCPServerConfig) string {
	if transport := NormalizeMCPTransportType(server.Type); transport != "" {
		return transport
	}
	if server.URL != "" {
		return "sse"
	}
	if server.Command != "" {
		return "stdio"
	}
	return ""
}
