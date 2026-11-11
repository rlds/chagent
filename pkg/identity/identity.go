// Package identity 为 Chagent 提供统一的用户身份工具。
// 它引入了规范的 "platform:id" 格式及匹配逻辑，
// 并与所有旧版允许列表格式保持向后兼容。
package identity

import (
	"strings"

	"github.com/rlds/chagent/pkg/bus"
)

// BuildCanonicalID 构造规范的 "platform:id" 标识符。
// platform 与 platformID 都会转为小写并去除首尾空白。
func BuildCanonicalID(platform, platformID string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	id := strings.TrimSpace(platformID)
	if p == "" || id == "" {
		return ""
	}
	return p + ":" + id
}

// ParseCanonicalID 将规范 ID（"platform:id"）拆分为各个部分。
// 若输入中不含冒号分隔符，则返回 ok=false。
func ParseCanonicalID(canonical string) (platform, id string, ok bool) {
	canonical = strings.TrimSpace(canonical)
	idx := strings.Index(canonical, ":")
	if idx <= 0 || idx == len(canonical)-1 {
		return "", "", false
	}
	return canonical[:idx], canonical[idx+1:], true
}

// MatchAllowed 检查给定发送者是否匹配单条允许列表条目。
// 它与所有旧版格式保持向后兼容：
//
//   - "123456"              → 匹配 sender.PlatformID
//   - "@alice"              → 匹配 sender.Username
//   - "123456|alice"        → 匹配 PlatformID 或 Username
//   - "telegram:123456"     → 与 sender.CanonicalID 精确匹配
func MatchAllowed(sender bus.SenderInfo, allowed string) bool {
	allowed = strings.TrimSpace(allowed)
	if allowed == "" {
		return false
	}

	// 优先尝试规范格式匹配："platform:id"
	if platform, id, ok := ParseCanonicalID(allowed); ok {
		// 仅当平台部分看起来像已知平台名时才按规范格式处理
		//（而不是纯数字字符串，后者可能是复合 ID）
		if !isNumeric(platform) {
			candidate := BuildCanonicalID(platform, id)
			if candidate != "" && sender.CanonicalID != "" {
				return strings.EqualFold(sender.CanonicalID, candidate)
			}
			// 若 sender 没有规范 ID，则尝试匹配 platform + platformID
			return strings.EqualFold(platform, sender.Platform) &&
				sender.PlatformID == id
		}
	}

	// 记录是否为显式用户名格式
	isAtUsername := strings.HasPrefix(allowed, "@")

	// 去除前导 "@" 以便进行用户名匹配
	trimmed := strings.TrimPrefix(allowed, "@")

	// 拆分复合的 "id|username" 格式
	allowedID := trimmed
	allowedUser := ""
	if idx := strings.Index(trimmed, "|"); idx > 0 {
		allowedID = trimmed[:idx]
		allowedUser = trimmed[idx+1:]
	}

	// 与 PlatformID 匹配
	if sender.PlatformID != "" && sender.PlatformID == allowedID {
		return true
	}

	// 仅当通过 "@username" 显式指定时才与 Username 匹配
	if isAtUsername && sender.Username != "" && sender.Username == trimmed {
		return true
	}

	// 将复合发送者格式与允许的各部分进行匹配
	if allowedUser != "" && sender.PlatformID != "" && sender.PlatformID == allowedID {
		return true
	}
	if allowedUser != "" && sender.Username != "" && sender.Username == allowedUser {
		return true
	}

	return false
}

// isNumeric 在 s 完全由数字组成时返回 true，允许可选的前导负号
// （Telegram 群组/频道 ID 如 -1001234567890 需要该负号）。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	start := 0
	if s[0] == '-' && len(s) > 1 {
		start = 1
	}
	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
