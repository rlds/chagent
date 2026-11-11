// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

// Package common 提供多个 LLM provider 实现（openai_compat、azure 等）共用的工具。
package common

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// 重新导出各 provider 共用的协议类型。
type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
	ExtraContent           = protocoltypes.ExtraContent
	GoogleExtra            = protocoltypes.GoogleExtra
	ReasoningDetail        = protocoltypes.ReasoningDetail
)

const DefaultRequestTimeout = 120 * time.Second

// NewHTTPClient 创建一个带可选代理和默认超时时间的 *http.Client。
func NewHTTPClient(proxy string) *http.Client {
	client := &http.Client{
		Timeout: DefaultRequestTimeout,
	}
	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err == nil {
			// 保留 http.DefaultTransport 的设置（TLS、HTTP/2、超时等）
			if base, ok := http.DefaultTransport.(*http.Transport); ok {
				tr := base.Clone()
				tr.Proxy = http.ProxyURL(parsed)
				client.Transport = tr
			} else {
				// 回退：若 DefaultTransport 不是 *http.Transport，则使用最小化的 transport。
				client.Transport = &http.Transport{
					Proxy: http.ProxyURL(parsed),
				}
			}
		} else {
			log.Printf("common: invalid proxy URL %q: %v", proxy, err)
		}
	}
	return client
}

// --- 消息序列化 ---

// openaiMessage 是 OpenAI 兼容 API 的传输格式消息。
// 它与 protocoltypes.Message 对应，但省略了 SystemParts——这是一个
// 内部字段，第三方端点无法识别。
type openaiMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type,omitempty"`
	Function *openaiFunctionCall `json:"function,omitempty"`
}

type openaiFunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// SerializeMessages 将内部 Message 结构体转换为 OpenAI 传输格式。
//   - 去除 SystemParts（第三方端点无法识别）
//   - 将带 Media 的消息转换为多段内容格式（text + image_url 片段）
//   - 为所有消息保留 ToolCallID、ToolCalls 和 ReasoningContent
func SerializeMessages(messages []Message) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		toolCalls := serializeToolCalls(m.ToolCalls)
		if len(m.Media) == 0 {
			out = append(out, openaiMessage{
				Role:             m.Role,
				Content:          m.Content,
				ReasoningContent: m.ReasoningContent,
				ToolCalls:        toolCalls,
				ToolCallID:       m.ToolCallID,
			})
			continue
		}

		// 带媒体消息的多段内容格式
		parts := make([]map[string]any, 0, 1+len(m.Media))
		if m.Content != "" {
			parts = append(parts, map[string]any{
				"type": "text",
				"text": m.Content,
			})
		}
		for _, mediaURL := range m.Media {
			if strings.HasPrefix(mediaURL, "data:image/") {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": mediaURL,
					},
				})
				continue
			}

			if format, data, ok := ParseDataAudioURL(mediaURL); ok {
				parts = append(parts, map[string]any{
					"type": "input_audio",
					"input_audio": map[string]any{
						"data":   data,
						"format": format,
					},
				})
			}
		}

		msg := map[string]any{
			"role":    m.Role,
			"content": parts,
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if m.ReasoningContent != "" {
			msg["reasoning_content"] = m.ReasoningContent
		}
		out = append(out, msg)
	}
	return out
}

func serializeToolCalls(toolCalls []ToolCall) []openaiToolCall {
	if len(toolCalls) == 0 {
		return nil
	}

	out := make([]openaiToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		wireCall := openaiToolCall{
			ID:   tc.ID,
			Type: tc.Type,
		}

		if tc.Function != nil {
			thoughtSignature := tc.Function.ThoughtSignature
			if thoughtSignature == "" {
				thoughtSignature = tc.ThoughtSignature
			}
			if thoughtSignature == "" && tc.ExtraContent != nil && tc.ExtraContent.Google != nil {
				thoughtSignature = tc.ExtraContent.Google.ThoughtSignature
			}
			wireCall.Function = &openaiFunctionCall{
				Name:             tc.Function.Name,
				Arguments:        tc.Function.Arguments,
				ThoughtSignature: thoughtSignature,
			}
		} else if tc.Name != "" || len(tc.Arguments) > 0 || tc.ThoughtSignature != "" {
			thoughtSignature := tc.ThoughtSignature
			if thoughtSignature == "" && tc.ExtraContent != nil && tc.ExtraContent.Google != nil {
				thoughtSignature = tc.ExtraContent.Google.ThoughtSignature
			}
			argsJSON := "{}"
			if len(tc.Arguments) > 0 {
				if encoded, err := json.Marshal(tc.Arguments); err == nil {
					argsJSON = string(encoded)
				}
			}
			wireCall.Function = &openaiFunctionCall{
				Name:             tc.Name,
				Arguments:        argsJSON,
				ThoughtSignature: thoughtSignature,
			}
		}

		out = append(out, wireCall)
	}

	return out
}

// ParseDataAudioURL 从 data:audio/... URL 中提取格式和 base64 数据。
func ParseDataAudioURL(mediaURL string) (format, data string, ok bool) {
	if !strings.HasPrefix(mediaURL, "data:audio/") {
		return "", "", false
	}

	payload := strings.TrimPrefix(mediaURL, "data:audio/")
	meta, data, found := strings.Cut(payload, ",")
	if !found {
		return "", "", false
	}

	format, _, _ = strings.Cut(meta, ";")
	format = strings.TrimSpace(format)
	data = strings.TrimSpace(data)
	if format == "" || data == "" {
		return "", "", false
	}
	return format, data, true
}

// --- 响应解析 ---

// ParseResponse 将 JSON 聊天补全响应体解析为 LLMResponse。
func ParseResponse(body io.Reader) (*LLMResponse, error) {
	var apiResponse struct {
		Choices []struct {
			Message struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				Reasoning        string            `json:"reasoning"`
				ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function *struct {
						Name             string          `json:"name"`
						Arguments        json.RawMessage `json:"arguments"`
						ThoughtSignature string          `json:"thought_signature"`
					} `json:"function"`
					ExtraContent *struct {
						Google *struct {
							ThoughtSignature string `json:"thought_signature"`
						} `json:"google"`
						ToolFeedbackExplanation string `json:"tool_feedback_explanation"`
					} `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *RawUsageInfo `json:"usage"`
		Model string        `json:"model"`
	}

	if err := json.NewDecoder(body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// 将原始的嵌套 usage 结构映射为扁平的 UsageInfo
	var usage *UsageInfo
	if apiResponse.Usage != nil {
		usage = apiResponse.Usage.ToUsageInfo()
	}

	if len(apiResponse.Choices) == 0 {
		return &LLMResponse{
			Content:      "",
			FinishReason: "stop",
		}, nil
	}

	choice := apiResponse.Choices[0]
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		arguments := make(map[string]any)
		name := ""

		thoughtSignature := ""
		if tc.Function != nil {
			thoughtSignature = tc.Function.ThoughtSignature
		}
		if thoughtSignature == "" && tc.ExtraContent != nil && tc.ExtraContent.Google != nil {
			thoughtSignature = tc.ExtraContent.Google.ThoughtSignature
		}

		if tc.Function != nil {
			name = tc.Function.Name
			arguments = DecodeToolCallArguments(tc.Function.Arguments, name)
		}

		toolCall := ToolCall{
			ID:               tc.ID,
			Name:             name,
			Arguments:        arguments,
			ThoughtSignature: thoughtSignature,
		}

		if thoughtSignature != "" || tc.ExtraContent != nil {
			extraContent := &ExtraContent{
				ToolFeedbackExplanation: "",
			}
			if tc.ExtraContent != nil {
				extraContent.ToolFeedbackExplanation = tc.ExtraContent.ToolFeedbackExplanation
			}
			if thoughtSignature != "" {
				extraContent.Google = &GoogleExtra{
					ThoughtSignature: thoughtSignature,
				}
			}
			if extraContent.Google != nil || strings.TrimSpace(extraContent.ToolFeedbackExplanation) != "" {
				toolCall.ExtraContent = extraContent
			}
		}

		toolCalls = append(toolCalls, toolCall)
	}

	return &LLMResponse{
		Content:          choice.Message.Content,
		ReasoningContent: choice.Message.ReasoningContent,
		Reasoning:        choice.Message.Reasoning,
		ReasoningDetails: choice.Message.ReasoningDetails,
		ToolCalls:        toolCalls,
		FinishReason:     normalizeFinishReason(choice.FinishReason),
		Usage:            usage,
		Model:            apiResponse.Model,
	}, nil
}

// normalizeFinishReason 统一各 provider 的 finish_reason 取值。
// 将 "length" 转换为 "truncated" 以便一致处理。
func normalizeFinishReason(reason string) string {
	if reason == "length" {
		return "truncated"
	}
	return reason
}

// DecodeToolCallArguments 从原始 JSON 中解码工具调用的参数。
func DecodeToolCallArguments(raw json.RawMessage, name string) map[string]any {
	arguments := make(map[string]any)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return arguments
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		log.Printf("common: failed to decode tool call arguments payload for %q: %v", name, err)
		arguments["raw"] = string(raw)
		return arguments
	}

	switch v := decoded.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return arguments
		}
		if err := json.Unmarshal([]byte(v), &arguments); err != nil {
			log.Printf("common: failed to decode tool call arguments for %q: %v", name, err)
			arguments["raw"] = v
		}
		return arguments
	case map[string]any:
		return v
	default:
		log.Printf("common: unsupported tool call arguments type for %q: %T", name, decoded)
		arguments["raw"] = string(raw)
		return arguments
	}
}

// --- HTTP 响应辅助函数 ---

// HandleErrorResponse 读取非 200 响应体并返回适当的错误。
func HandleErrorResponse(resp *http.Response, apiBase string) error {
	contentType := resp.Header.Get("Content-Type")
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256))
	if readErr != nil {
		return fmt.Errorf("failed to read response: %w", readErr)
	}
	if LooksLikeHTML(body, contentType) {
		return WrapHTMLResponseError(resp.StatusCode, body, contentType, apiBase)
	}
	return fmt.Errorf(
		"API request failed:\n  Status: %d\n  Body:   %s",
		resp.StatusCode,
		ResponsePreview(body, 128),
	)
}

// ReadAndParseResponse 先窥探响应体以检测 HTML 错误，
// 再将 JSON 响应解析为 LLMResponse。
func ReadAndParseResponse(resp *http.Response, apiBase string) (*LLMResponse, error) {
	contentType := resp.Header.Get("Content-Type")
	reader := bufio.NewReader(resp.Body)
	prefix, err := reader.Peek(256)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, fmt.Errorf("failed to inspect response: %w", err)
	}
	if LooksLikeHTML(prefix, contentType) {
		return nil, WrapHTMLResponseError(resp.StatusCode, prefix, contentType, apiBase)
	}
	out, err := ParseResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %w", err)
	}
	return out, nil
}

// LooksLikeHTML 检查响应体是否看起来像 HTML。
func LooksLikeHTML(body []byte, contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml") {
		return true
	}
	prefix := bytes.ToLower(leadingTrimmedPrefix(body, 128))
	return bytes.HasPrefix(prefix, []byte("<!doctype html")) ||
		bytes.HasPrefix(prefix, []byte("<html")) ||
		bytes.HasPrefix(prefix, []byte("<head")) ||
		bytes.HasPrefix(prefix, []byte("<body"))
}

// WrapHTMLResponseError 为 HTML 响应创建描述性错误。
func WrapHTMLResponseError(statusCode int, body []byte, contentType, apiBase string) error {
	respPreview := ResponsePreview(body, 128)
	return fmt.Errorf(
		"API request failed: %s returned HTML instead of JSON (content-type: %s); check api_base or proxy configuration.\n  Status: %d\n  Body:   %s",
		apiBase,
		contentType,
		statusCode,
		respPreview,
	)
}

// ResponsePreview 返回响应体的截断预览，用于错误消息。
func ResponsePreview(body []byte, maxLen int) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "<empty>"
	}
	if len(trimmed) <= maxLen {
		return string(trimmed)
	}
	return string(trimmed[:maxLen]) + "..."
}

func leadingTrimmedPrefix(body []byte, maxLen int) []byte {
	i := 0
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			i++
		default:
			end := i + maxLen
			if end > len(body) {
				end = len(body)
			}
			return body[i:end]
		}
	}
	return nil
}

// --- 数值辅助函数 ---

// AsInt 将各种数值类型转换为 int。
func AsInt(v any) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		return int(val), true
	case float32:
		return int(val), true
	default:
		return 0, false
	}
}

// AsFloat 将各种数值类型转换为 float64。
func AsFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	default:
		return 0, false
	}
}

// RawUsageInfo 匹配 OpenAI 兼容 API（DeepSeek、GLM 等）返回的
// 嵌套 usage JSON 结构。它包含 prompt_tokens_details 和
// completion_tokens_details，provider 会将其展平到 UsageInfo 中。
type RawUsageInfo struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	PromptTokensDetails     *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	// DeepSeek 专有字段
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
}

// ToUsageInfo 将嵌套的原始 usage 结构转换为扁平的 UsageInfo 类型。
func (r *RawUsageInfo) ToUsageInfo() *UsageInfo {
	u := &UsageInfo{
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
	}
	if r.PromptTokensDetails != nil {
		u.CachedTokens = r.PromptTokensDetails.CachedTokens
	}
	if r.CompletionTokensDetails != nil {
		u.ReasoningTokens = r.CompletionTokensDetails.ReasoningTokens
	}
	// 若值更大，则 DeepSeek 风格的字段优先
	if r.PromptCacheHitTokens > u.CachedTokens {
		u.CachedTokens = r.PromptCacheHitTokens
	}
	return u
}
