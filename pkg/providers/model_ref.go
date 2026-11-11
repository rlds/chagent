package providers

import "strings"

// ModelRef 表示一个解析后的模型引用，包含 provider 和模型名。
type ModelRef struct {
	Provider string
	Model    string
}

// ParseModelRef 将 "anthropic/claude-opus" 解析为 {Provider: "anthropic", Model: "claude-opus"}。
// 若其中没有斜杠，则使用 defaultProvider。
// 输入为空时返回 nil。
func ParseModelRef(raw string, defaultProvider string) *ModelRef {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	provider, model := SplitModelProviderAndID(raw, defaultProvider)
	if model == "" {
		return nil
	}
	return &ModelRef{
		Provider: provider,
		Model:    model,
	}
}

// NormalizeProvider 将 provider 标识规范化为标准形式。
func NormalizeProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return ""
	}
	if canonical, ok := normalizedModelProviderAliasesByName[normalized]; ok {
		return canonical
	}
	return normalized
}

// ModelKey 返回用于去重的标准 "provider/model" 键。
func ModelKey(provider, model string) string {
	return NormalizeProvider(provider) + "/" + strings.ToLower(strings.TrimSpace(model))
}
