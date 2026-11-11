// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package anthropicmessages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/providers/common"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
)

const (
	defaultAPIVersion     = "2023-06-01"
	defaultBaseURL        = "https://api.anthropic.com/v1"
	defaultRequestTimeout = 120 * time.Second

	// AuthSchemeXAPIKey 是使用 X-API-Key 请求头的标准 Anthropic 认证方式。
	AuthSchemeXAPIKey = "x-api-key"
	// AuthSchemeBearer 对私有网关使用 Authorization: Bearer <token>。
	AuthSchemeBearer = "bearer"
)

// Provider 通过 HTTP 实现 Anthropic Messages API（不使用 SDK）。
// 它支持使用 Anthropic 原生消息格式的自定义端点。
type Provider struct {
	apiKey     string
	apiBase    string
	authScheme string // "x-api-key"（默认）或 "bearer"
	httpClient *http.Client
	userAgent  string
}

// NewProvider 创建一个新的 Anthropic Messages API provider。
func NewProvider(apiKey, apiBase, userAgent string) *Provider {
	return NewProviderWithTimeout(apiKey, apiBase, userAgent, 0)
}

// NewProviderWithTimeout 创建带自定义请求超时的 provider。
func NewProviderWithTimeout(apiKey, apiBase, userAgent string, timeoutSeconds int) *Provider {
	return NewProviderWithAuthScheme(apiKey, apiBase, userAgent, timeoutSeconds, AuthSchemeXAPIKey)
}

// NewProviderWithAuthScheme 创建使用指定认证方式的 provider。
// 对于接受 Authorization: Bearer <token> 而非标准 X-API-Key 请求头的
// 私有网关，请使用 AuthSchemeBearer。
func NewProviderWithAuthScheme(
	apiKey, apiBase, userAgent string,
	timeoutSeconds int,
	authScheme string,
) *Provider {
	baseURL := common.NormalizeBaseURL(apiBase, defaultBaseURL, true)
	timeout := defaultRequestTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	if authScheme == "" {
		authScheme = AuthSchemeXAPIKey
	}

	return &Provider{
		apiKey:     apiKey,
		apiBase:    baseURL,
		authScheme: authScheme,
		userAgent:  userAgent,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Chat 将消息发送到 Anthropic Messages API 并返回响应。
func (p *Provider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("API key not configured")
	}

	// 构建请求体
	requestBody, err := buildRequestBody(messages, tools, model, options)
	if err != nil {
		return nil, fmt.Errorf("building request body: %w", err)
	}

	// 序列化为 JSON
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("serializing request body: %w", err)
	}

	// 构建请求 URL
	endpointURL, err := url.JoinPath(p.apiBase, "messages")
	if err != nil {
		return nil, fmt.Errorf("building endpoint URL: %w", err)
	}

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", endpointURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", defaultAPIVersion)

	// 根据认证方式设置认证请求头
	switch p.authScheme {
	case AuthSchemeBearer:
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	default:
		req.Header.Set("X-API-Key", p.apiKey) //nolint:canonicalheader // Anthropic API 要求使用精确的请求头名称
	}

	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}

	// 执行请求
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	// 检查 HTTP 错误并给出详细信息
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("authentication failed (401): check your API key")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limited (429): %s", string(body))
	case http.StatusBadRequest:
		return nil, fmt.Errorf("bad request (400): %s", string(body))
	case http.StatusNotFound:
		return nil, fmt.Errorf("endpoint not found (404): %s", string(body))
	case http.StatusInternalServerError:
		return nil, fmt.Errorf("internal server error (500): %s", string(body))
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("service unavailable (503): %s", string(body))
	default:
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
		}
	}

	// 解析响应
	return parseResponseBody(body)
}

// GetDefaultModel 返回此 provider 的默认模型。
func (p *Provider) GetDefaultModel() string {
	return "claude-sonnet-4.6"
}

// buildRequestBody 将内部消息格式转换为 Anthropic Messages API 格式。
func buildRequestBody(
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (map[string]any, error) {
	// max_tokens 是必填项，且由 agent 循环保证提供
	maxTokens, ok := common.AsInt(options["max_tokens"])
	if !ok {
		return nil, fmt.Errorf("max_tokens is required in options")
	}

	result := map[string]any{
		"model":      model,
		"max_tokens": int64(maxTokens),
		"messages":   []any{},
	}

	// 从 options 中设置 temperature
	if temp, ok := common.AsFloat(options["temperature"]); ok {
		result["temperature"] = temp
	}

	// 处理消息
	var systemPrompt string
	var apiMessages []any

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// 累积系统消息
			if systemPrompt != "" {
				systemPrompt += "\n\n" + msg.Content
			} else {
				systemPrompt = msg.Content
			}

		case "user":
			if msg.ToolCallID != "" {
				// 工具结果消息 —— 若上一条用户消息包含 tool_results，则合并到其中
				toolResultBlock := map[string]any{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     msg.Content,
				}
				if len(apiMessages) > 0 {
					if prev, ok := apiMessages[len(apiMessages)-1].(map[string]any); ok && prev["role"] == "user" {
						if content, ok := prev["content"].([]map[string]any); ok {
							prev["content"] = append(content, toolResultBlock)
							continue
						}
					}
				}
				apiMessages = append(apiMessages, map[string]any{
					"role":    "user",
					"content": []map[string]any{toolResultBlock},
				})
			} else {
				// 普通用户消息
				apiMessages = append(apiMessages, map[string]any{
					"role":    "user",
					"content": msg.Content,
				})
			}

		case "assistant":
			content := []any{}

			// 若存在文本内容则添加
			if msg.Content != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}

			// 添加 tool_use 块
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.Name) == "" {
					continue
				}

				// 处理为 nil 的 Arguments（GLM-4 可能返回 null input）
				input := tc.Arguments
				if input == nil {
					input = map[string]any{}
				}

				toolUse := map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": input,
				}
				content = append(content, toolUse)
			}

			apiMessages = append(apiMessages, map[string]any{
				"role":    "assistant",
				"content": content,
			})

		case "tool":
			// 工具结果（另一种格式）—— 若上一条用户消息包含 tool_results，则合并到其中
			toolResultBlock := map[string]any{
				"type":        "tool_result",
				"tool_use_id": msg.ToolCallID,
				"content":     msg.Content,
			}
			if len(apiMessages) > 0 {
				if prev, ok := apiMessages[len(apiMessages)-1].(map[string]any); ok && prev["role"] == "user" {
					if content, ok := prev["content"].([]map[string]any); ok {
						prev["content"] = append(content, toolResultBlock)
						continue
					}
				}
			}
			apiMessages = append(apiMessages, map[string]any{
				"role":    "user",
				"content": []map[string]any{toolResultBlock},
			})
		}
	}

	result["messages"] = apiMessages

	// 若存在系统提示词则设置
	if systemPrompt != "" {
		result["system"] = systemPrompt
	}

	// 若存在工具则添加
	if len(tools) > 0 {
		result["tools"] = buildTools(tools)
	}

	return result, nil
}

// buildTools 将工具定义转换为 Anthropic 格式。
func buildTools(tools []ToolDefinition) []any {
	result := make([]any, len(tools))
	for i, tool := range tools {
		toolDef := map[string]any{
			"name":         tool.Function.Name,
			"description":  tool.Function.Description,
			"input_schema": tool.Function.Parameters,
		}
		result[i] = toolDef
	}
	return result
}

// parseResponseBody 解析 Anthropic Messages API 响应。
func parseResponseBody(body []byte) (*LLMResponse, error) {
	var resp anthropicMessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing JSON response: %w", err)
	}

	// 提取内容与工具调用
	var content strings.Builder
	toolCalls := make([]ToolCall, 0) // 初始化为空切片（而非 nil），以保证 JSON 序列化结果一致

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			argsJSON, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: block.Input,
				Function: &FunctionCall{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	// 映射 stop_reason
	finishReason := "stop"
	switch resp.StopReason {
	case "tool_use":
		finishReason = "tool_calls"
	case "max_tokens":
		finishReason = "length"
	case "end_turn":
		finishReason = "stop"
	case "stop_sequence":
		finishReason = "stop"
	}

	return &LLMResponse{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Model:        resp.Model,
		Usage: &UsageInfo{
			// Anthropic 的 InputTokens 不含缓存创建/读取的 token。
			// 我们将它们相加，以匹配 OpenAI 兼容的 "prompt_tokens" 语义。
			PromptTokens:             int(resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens),
			CompletionTokens:         int(resp.Usage.OutputTokens),
			TotalTokens:              int(resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.OutputTokens),
			CacheReadInputTokens:     int(resp.Usage.CacheReadInputTokens),
			CacheCreationInputTokens: int(resp.Usage.CacheCreationInputTokens),
			// 将 Anthropic 风格的缓存读取映射为 CachedTokens，以保持跨 provider 一致
			CachedTokens: int(resp.Usage.CacheReadInputTokens),
		},
	}, nil
}

// Anthropic API 响应结构

type anthropicMessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Model      string         `json:"model"`
	Usage      usageInfo      `json:"usage"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

type usageInfo struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}
