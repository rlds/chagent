package utils

import (
	"strings"
	"sync/atomic"
	"unicode"
)

// 用于禁用截断的全局变量
var disableTruncation atomic.Bool

// SetDisableTruncation 全局启用或禁用字符串截断
func SetDisableTruncation(enabled bool) {
	disableTruncation.Store(enabled)
}

// SanitizeMessageContent 移除 Unicode 控制字符、格式字符（RTL 覆盖符、
// 零宽字符）以及其他可能干扰 LLM 或导致智能体 UI 显示异常的非图形字符。
func SanitizeMessageContent(input string) string {
	var sb strings.Builder
	// 预分配内存以避免多次分配
	sb.Grow(len(input))

	for _, r := range input {
		// unicode.IsGraphic 在该 rune 属于 Unicode 图形字符时返回 true。
		// 这包括字母、标记、数字、标点和符号。
		// 不包括控制字符（Cc）、格式字符（Cf）、
		// 代理项（Cs）和私有使用区（Co）。
		if unicode.IsGraphic(r) || r == '\n' || r == '\r' || r == '\t' {
			sb.WriteRune(r)
		}
	}

	return sb.String()
}

// Truncate 返回 s 的截断版本，最多包含 maxLen 个 rune。
// 能正确处理多字节 Unicode 字符。
// 如果字符串被截断，会追加 "..." 以表示截断。
func Truncate(s string, maxLen int) string {
	// 如果禁截断标志已启用，则返回完整字符串
	if disableTruncation.Load() {
		return s
	}
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	// 为 "..." 预留 3 个字符
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// DerefStr 解引用字符串指针，
// 返回其值；若指针为 nil 则返回回退值。
func DerefStr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
