package utils

import (
	"fmt"
	"strings"
)

// ValidateSkillIdentifier 校验给定的技能标识符（slug 或注册名）非空，
// 并且出于安全考虑不包含路径分隔符（"/"、"\\"）或 ".."。
func ValidateSkillIdentifier(identifier string) error {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return fmt.Errorf("identifier is required and must be a non-empty string")
	}
	if strings.ContainsAny(trimmed, "/\\") || strings.Contains(trimmed, "..") {
		return fmt.Errorf("identifier must not contain path separators or '..' to prevent directory traversal")
	}
	return nil
}
