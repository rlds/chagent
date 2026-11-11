package providers

import (
	"context"
	"fmt"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	StreamChunk            = protocoltypes.StreamChunk
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
	ExtraContent           = protocoltypes.ExtraContent
	GoogleExtra            = protocoltypes.GoogleExtra
	ContentBlock           = protocoltypes.ContentBlock
	CacheControl           = protocoltypes.CacheControl
	Attachment             = protocoltypes.Attachment
)

type LLMProvider interface {
	Chat(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
	) (*LLMResponse, error)
	GetDefaultModel() string
}

type StatefulProvider interface {
	LLMProvider
	Close()
}

// StreamingProvider 是支持 token 流式输出的 provider 的可选接口。
// onChunk 接收截至目前累积的文本（而非单个增量）。
// 返回的 LLMResponse 仍是完整的响应，以兼容工具调用处理。
type StreamingProvider interface {
	ChatStream(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
		onChunk func(accumulated string),
	) (*LLMResponse, error)
}

type StreamingEventProvider interface {
	ChatStreamEvents(
		ctx context.Context,
		messages []Message,
		tools []ToolDefinition,
		model string,
		options map[string]any,
		onChunk func(StreamChunk),
	) (*LLMResponse, error)
}

// ThinkingCapable 是支持扩展思考的 provider（例如 Anthropic）的可选接口。
// agent 循环用它来在配置了 thinking_level、但当前 provider 无法使用
// 该能力时给出警告。
type ThinkingCapable interface {
	SupportsThinking() bool
}

// NativeSearchCapable 是支持在 LLM 推理期间使用内建网页搜索的
// provider（例如 OpenAI web_search_preview、xAI Grok search）的可选接口。
// 当当前 provider 实现该接口且返回 true 时，agent 循环可以隐藏
// 客户端侧的 web_search 工具，以避免重复的搜索入口，改用
// provider 的原生搜索。
type NativeSearchCapable interface {
	SupportsNativeSearch() bool
}

// FailoverReason 对 LLM 请求失败的原因进行分类，用于故障转移决策。
type FailoverReason string

const (
	FailoverAuth            FailoverReason = "auth"
	FailoverRateLimit       FailoverReason = "rate_limit"
	FailoverBilling         FailoverReason = "billing"
	FailoverNetwork         FailoverReason = "network"
	FailoverTimeout         FailoverReason = "timeout"
	FailoverFormat          FailoverReason = "format"
	FailoverContextOverflow FailoverReason = "context_overflow"
	FailoverOverloaded      FailoverReason = "overloaded"
	FailoverUnknown         FailoverReason = "unknown"
)

// FailoverError 为 LLM provider 错误附带分类元数据。
type FailoverError struct {
	Reason   FailoverReason
	Provider string
	Model    string
	Status   int
	Wrapped  error
}

func (e *FailoverError) Error() string {
	return fmt.Sprintf("failover(%s): provider=%s model=%s status=%d: %v",
		e.Reason, e.Provider, e.Model, e.Status, e.Wrapped)
}

func (e *FailoverError) Unwrap() error {
	return e.Wrapped
}

// IsRetriable 返回该错误是否应触发回退到下一个候选。
// 不可重试的情况：格式错误（请求结构错误、图片尺寸/大小问题）。
func (e *FailoverError) IsRetriable() bool {
	return e.Reason != FailoverFormat && e.Reason != FailoverContextOverflow
}

// ModelConfig 保存主模型和回退列表。
type ModelConfig struct {
	Primary   string
	Fallbacks []string
}
