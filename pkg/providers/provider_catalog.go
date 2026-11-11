package providers

import (
	"sort"
	"strings"
)

// ModelProviderOptions 返回暴露给 Web UI 的标准 provider 目录。
func ModelProviderOptions() []ModelProviderOption {
	options := make([]ModelProviderOption, 0, len(modelProviderOptionsByName))
	for _, option := range modelProviderOptionsByName {
		options = append(options, option)
	}
	sort.Slice(options, func(i, j int) bool {
		return options[i].ID < options[j].ID
	})
	return options
}

// IsSupportedModelProvider 报告 provider 是否能解析为
// ModelProviderOptions 返回的某个 provider ID。
func IsSupportedModelProvider(provider string) bool {
	_, ok := modelProviderOptionForName(provider)
	return ok
}

// IsModelProviderFetchable 报告 provider 是否支持通过启动器
// fetch 端点列出上游 /models。
func IsModelProviderFetchable(provider string) bool {
	option, ok := modelProviderOptionForName(provider)
	return ok && option.SupportsFetch
}

// IsCreatableModelProvider 报告 provider 是否可在 Web UI 中
// 被选作新建的模型条目。
func IsCreatableModelProvider(provider string) bool {
	option, ok := modelProviderOptionForName(provider)
	return ok && option.CreateAllowed
}

// IsDefaultModelProvider 报告 provider 是否可用作默认聊天模型。
// 某些 provider（例如仅支持 ASR 的条目）会有意出现在 model_list
// 管理中，但不能作为网关默认模型。
func IsDefaultModelProvider(provider string) bool {
	option, ok := modelProviderOptionForName(provider)
	return ok && option.DefaultModelAllowed
}

// SplitModelProviderAndID 将遗留的 "provider/model" 字符串拆分为
// 实际生效的 provider 和标准模型 ID。无法识别的前缀会被视为
// 模型 ID 的一部分，并回退到 defaultProvider。
func SplitModelProviderAndID(model, defaultProvider string) (provider, modelID string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}

	provider, modelID = splitKnownProviderModel(model)
	if provider != "" || modelID != "" {
		return provider, modelID
	}

	return NormalizeProvider(defaultProvider), model
}

func splitKnownProviderModel(model string) (provider, modelID string) {
	provider, modelID, found := strings.Cut(strings.TrimSpace(model), "/")
	if !found {
		return "", ""
	}
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	if provider == "" {
		return "", modelID
	}
	if !IsSupportedModelProvider(provider) {
		return "", ""
	}
	return NormalizeProvider(provider), modelID
}
