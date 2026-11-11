package common

import "strings"

// NormalizeBaseURL 确保 Anthropic 基础 URL 的格式正确。
// 若存在末尾的 /v1 后缀则先将其移除（以避免重复），然后在 appendV1Suffix
// 为 true 时重新追加 /v1。apiBase 为空时回退到 defaultBaseURL。
func NormalizeBaseURL(apiBase, defaultBaseURL string, appendV1Suffix bool) string {
	base := strings.TrimSpace(apiBase)
	if base == "" {
		return defaultBaseURL
	}

	base = strings.TrimRight(base, "/")
	if before, ok := strings.CutSuffix(base, "/v1"); ok {
		base = before
	}
	if base == "" {
		return defaultBaseURL
	}

	if appendV1Suffix {
		return base + "/v1"
	}
	return base
}
