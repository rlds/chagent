// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package providers

import (
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/config"
	anthropicmessages "github.com/rlds/chagent/pkg/providers/anthropic_messages"
)

// defaultAnthropicAPIBase 是模型配置未设置 api_base 时使用的原生
// Anthropic Messages 端点。
const defaultAnthropicAPIBase = "https://api.anthropic.com/v1"

// ExtractProtocol 从模型配置中提取生效的协议和模型标识符。
//
// 显式的 Provider 字段优先。当 Provider 为空时，协议由 Model 推断；
// 纯模型名默认使用 "openai"。带 provider 前缀的模型会从返回的模型 ID
// 中去掉第一个以斜杠分隔的片段。
//
// 返回的协议会被规范化成该 provider 的标准拼写。
// 示例：
//   - Model "openai/gpt-4o" -> ("openai", "gpt-4o")
//   - Model "nvidia/z-ai/glm-5.1" -> ("nvidia", "z-ai/glm-5.1")
//   - Provider "nvidia", Model "z-ai/glm-5.1" -> ("nvidia", "z-ai/glm-5.1")
//   - Provider "openai", Model "openai/gpt-4o" -> ("openai", "openai/gpt-4o")
//   - Model "gpt-4o" -> ("openai", "gpt-4o")
func ExtractProtocol(cfg *config.ModelConfig) (protocol, modelID string) {
	if cfg == nil {
		return "", ""
	}

	model := strings.TrimSpace(cfg.Model)
	if provider := strings.TrimSpace(cfg.Provider); provider != "" {
		return NormalizeProvider(provider), model
	}
	return SplitModelProviderAndID(model, "openai")
}

// ResolveAPIBase 返回配置的 API 基础地址；当模型使用带已知默认端点的
// 基于 HTTP 的 provider 系列时，返回该协议的默认值。
func ResolveAPIBase(cfg *config.ModelConfig) string {
	if cfg == nil {
		return ""
	}
	if apiBase := strings.TrimSpace(cfg.APIBase); apiBase != "" {
		return strings.TrimRight(apiBase, "/")
	}
	protocol, _ := ExtractProtocol(cfg)
	return strings.TrimRight(getDefaultAPIBase(protocol), "/")
}

// CreateProviderFromConfig 根据 ModelConfig 创建 provider。
// 它使用 ExtractProtocol 判断应创建哪个 provider。
//
// 仅支持两种线路协议：
//   - 兼容 OpenAI Chat Completions 的协议（"openai" 以及下方 switch 中列出的
//     预设别名），使用 API key 认证；
//   - 兼容 Anthropic Messages 的协议（"anthropic"、"anthropic-messages"，以及
//     使用 "anthropic-bearer" 认证方案的变体）。
//
// 厂商专属协议（Azure responses API、Bedrock、Gemini、CLI 桥接、
// OAuth 托管的 provider 等）有意不支持；用户可通过通用协议上的
// api_base/extra_body 来表达厂商的特殊行为。
//
// 返回 provider、ExtractProtocol 得出的生效模型 ID，以及可能出现的错误。
func CreateProviderFromConfig(cfg *config.ModelConfig) (LLMProvider, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("config is nil")
	}

	if cfg.Model == "" {
		return nil, "", fmt.Errorf("model is required")
	}

	protocol, modelID := ExtractProtocol(cfg)

	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = fmt.Sprintf("Chagent/%s", config.Version)
	}

	switch protocol {
	case "openai":
		// 兼容 OpenAI Chat Completions 的端点，使用 API key 认证。
		if cfg.APIKey() == "" && cfg.APIBase == "" {
			return nil, "", fmt.Errorf("api_key or api_base is required for HTTP-based protocol %q", protocol)
		}
		apiBase := cfg.APIBase
		if apiBase == "" {
			apiBase = getDefaultAPIBase(protocol)
		}
		provider := NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(
			cfg.APIKey(),
			apiBase,
			cfg.Proxy,
			cfg.MaxTokensField,
			userAgent,
			cfg.RequestTimeout,
			cfg.ExtraBody,
			cfg.CustomHeaders,
		)
		provider.SetProviderName(protocol)
		return finalizeProviderFromConfig(provider, modelID, cfg)

	case "anthropic", "anthropic-messages":
		// 原生线路格式的 Anthropic Messages API（基于 HTTP，不依赖 SDK）。
		// "anthropic" 是 "anthropic-messages" 的兼容别名。
		apiBase := strings.TrimSpace(cfg.APIBase)
		if apiBase == "" {
			apiBase = defaultAnthropicAPIBase
		}
		if cfg.APIKey() == "" {
			return nil, "", fmt.Errorf("api_key is required for %q protocol (model: %s)", protocol, cfg.Model)
		}
		return finalizeProviderFromConfig(anthropicmessages.NewProviderWithTimeout(
			cfg.APIKey(),
			apiBase,
			userAgent,
			cfg.RequestTimeout,
		), modelID, cfg)

	case "anthropic-bearer":
		// 通过私有网关访问 Anthropic Messages API，这类网关使用
		// Authorization: Bearer <token> 而不是 X-API-Key。
		// api_keys 字段存放 bearer token。
		apiBase := strings.TrimSpace(cfg.APIBase)
		if apiBase == "" {
			apiBase = defaultAnthropicAPIBase
		}
		if cfg.APIKey() == "" {
			return nil, "", fmt.Errorf("api_key (bearer token) is required for anthropic-bearer protocol (model: %s)", cfg.Model)
		}
		return finalizeProviderFromConfig(anthropicmessages.NewProviderWithAuthScheme(
			cfg.APIKey(),
			apiBase,
			userAgent,
			cfg.RequestTimeout,
			anthropicmessages.AuthSchemeBearer,
		), modelID, cfg)

	case "litellm", "lmstudio", "gpt4free", "openrouter", "groq", "zhipu", "nvidia", "venice",
		"nearai", "ollama", "moonshot", "shengsuanyun", "siliconflow", "deepseek", "cerebras",
		"vivgrid", "volcengine", "vllm", "qwen-portal", "qwen-intl", "qwen-us", "mistral",
		"avian", "longcat", "modelscope", "novita", "zai", "mimo", "minimax", "alibaba-coding":
		// 兼容 OpenAI 的 OpenAI Chat Completions 端点。minimax 和
		// alibaba-coding 在这里只是普通预设：它们的请求特殊行为由用户
		// 通过 extra_body/api_base 表达，而不是由厂商专属的 provider 实现。
		if cfg.APIKey() == "" && cfg.APIBase == "" && !isEmptyAPIKeyAllowed(protocol) {
			return nil, "", fmt.Errorf("api_key or api_base is required for HTTP-based protocol %q", protocol)
		}
		apiBase := cfg.APIBase
		if apiBase == "" {
			apiBase = getDefaultAPIBase(protocol)
		}
		provider := NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(
			cfg.APIKey(),
			apiBase,
			cfg.Proxy,
			cfg.MaxTokensField,
			userAgent,
			cfg.RequestTimeout,
			cfg.ExtraBody,
			cfg.CustomHeaders,
		)
		provider.SetProviderName(protocol)
		return finalizeProviderFromConfig(provider, modelID, cfg)

	default:
		return nil, "", fmt.Errorf("unknown protocol %q in model %q", protocol, cfg.Model)
	}
}

func finalizeProviderFromConfig(
	provider LLMProvider,
	modelID string,
	cfg *config.ModelConfig,
) (LLMProvider, string, error) {
	wrapped, err := wrapProviderWithToolSchemaTransform(provider, cfg.ToolSchemaTransform)
	if err != nil {
		return nil, "", err
	}
	return wrapped, modelID, nil
}

func isEmptyAPIKeyAllowed(protocol string) bool {
	option, ok := modelProviderOptionForName(protocol)
	return ok && option.EmptyAPIKeyAllowed
}

// IsEmptyAPIKeyAllowedForProtocol 报告某协议在使用其默认本地端点时
// 是否允许不带 api_key 的请求。
func IsEmptyAPIKeyAllowedForProtocol(protocol string) bool {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	return isEmptyAPIKeyAllowed(protocol)
}

// IsHTTPAPIProtocol 报告某 provider 在模型配置路径中是否使用 HTTP API 基础地址。
// 只有通用的 OpenAI 兼容系列和 Anthropic Messages 系列会返回 true；
// 任何没有 HTTP 端点（或没有目录条目）的协议都返回 false。
func IsHTTPAPIProtocol(protocol string) bool {
	protocol = NormalizeProvider(protocol)
	option, ok := modelProviderOptionsByName[protocol]
	return ok && option.httpAPI
}

// DefaultAPIBaseForProtocol 返回某协议配置的默认 API 基础地址。
// 若该协议没有默认基础地址，则返回空字符串。
func DefaultAPIBaseForProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	return getDefaultAPIBase(protocol)
}

// getDefaultAPIBase 返回给定协议的默认 API 基础 URL。
func getDefaultAPIBase(protocol string) string {
	option, ok := modelProviderOptionForName(protocol)
	if !ok {
		return ""
	}
	return option.DefaultAPIBase
}
