package bus

import "github.com/rlds/chagent/pkg/providers/protocoltypes"

// SenderInfo 提供结构化的发送者身份信息。
type SenderInfo struct {
	Platform    string `json:"platform,omitempty"`     // "telegram"、"discord"、"slack" 等
	PlatformID  string `json:"platform_id,omitempty"`  // 平台原始 ID，例如 "123456"
	CanonicalID string `json:"canonical_id,omitempty"` // "platform:id" 格式
	Username    string `json:"username,omitempty"`     // 用户名（例如 @alice）
	DisplayName string `json:"display_name,omitempty"` // 显示名称
}

// InboundContext 记录一条入站消息经过规范化的、与平台无关的事实信息。
// 它是路由与会话分配的唯一事实来源。
type InboundContext struct {
	Channel string `json:"channel"`
	Account string `json:"account,omitempty"`

	ChatID   string `json:"chat_id"`
	ChatType string `json:"chat_type,omitempty"` // direct / group / channel
	TopicID  string `json:"topic_id,omitempty"`

	SpaceID   string `json:"space_id,omitempty"`
	SpaceType string `json:"space_type,omitempty"` // guild / team / workspace / tenant

	SenderID  string `json:"sender_id"`
	MessageID string `json:"message_id,omitempty"`

	Mentioned bool `json:"mentioned,omitempty"`

	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	ReplyToSenderID  string `json:"reply_to_sender_id,omitempty"`

	ReplyHandles map[string]string `json:"reply_handles,omitempty"`
	Raw          map[string]string `json:"raw,omitempty"`
}

type InboundMessage struct {
	Context    InboundContext `json:"context"`
	Sender     SenderInfo     `json:"sender"`
	Content    string         `json:"content"`
	Media      []string       `json:"media,omitempty"`
	MediaScope string         `json:"media_scope,omitempty"` // 媒体生命周期作用域
	SessionKey string         `json:"session_key"`

	// 从 Context 派生的便捷镜像字段，供运行时消费者使用。
	Channel   string `json:"channel"`
	SenderID  string `json:"sender_id"`
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id,omitempty"` // 平台消息 ID
}

// OutboundScope 记录与出站轮次结果关联的结构化会话作用域，
// 且不依赖 session 包。
type OutboundScope struct {
	Version    int               `json:"version,omitempty"`
	AgentID    string            `json:"agent_id,omitempty"`
	Channel    string            `json:"channel,omitempty"`
	Account    string            `json:"account,omitempty"`
	Dimensions []string          `json:"dimensions,omitempty"`
	Values     map[string]string `json:"values,omitempty"`
}

// ContextUsage 描述当前会话占用了模型上下文窗口的多少空间，
// 以及距离触发压缩还有多远。
type ContextUsage struct {
	UsedTokens        int `json:"used_tokens"`
	TotalTokens       int `json:"total_tokens"`        // 模型上下文窗口
	HistoryTokens     int `json:"history_tokens"`      // 仅历史消息 token（maybeSummarize 检查的项）
	CompressAtTokens  int `json:"compress_at_tokens"`  // 硬预算压缩阈值（contextWindow - maxTokens）
	SummarizeAtTokens int `json:"summarize_at_tokens"` // 软性摘要触发条件（相对历史 token）
	UsedPercent       int `json:"used_percent"`        // 0-100，相对于 compressAt
}

type OutboundMessage struct {
	Channel          string                   `json:"channel"`
	ChatID           string                   `json:"chat_id"`
	Context          InboundContext           `json:"context"`
	AgentID          string                   `json:"agent_id,omitempty"`
	SessionKey       string                   `json:"session_key,omitempty"`
	Scope            *OutboundScope           `json:"scope,omitempty"`
	Content          string                   `json:"content"`
	ReplyToMessageID string                   `json:"reply_to_message_id,omitempty"`
	ContextUsage     *ContextUsage            `json:"context_usage,omitempty"`
	Usage            *protocoltypes.UsageInfo `json:"usage,omitempty"` // LLM API 用量（包括缓存命中 token、推理 token 等）
}

// MediaPart 描述要发送的单个媒体附件。
type MediaPart struct {
	Type        string `json:"type"`                   // "image" | "audio" | "video" | "file"
	Ref         string `json:"ref"`                    // 媒体存储引用，例如 "media://abc123"
	Caption     string `json:"caption,omitempty"`      // 可选的说明文字
	Filename    string `json:"filename,omitempty"`     // 原始文件名提示
	ContentType string `json:"content_type,omitempty"` // MIME 类型提示
}

// OutboundMediaMessage 通过总线将媒体附件从 Agent 传递到各通道。
type OutboundMediaMessage struct {
	Channel    string         `json:"channel"`
	ChatID     string         `json:"chat_id"`
	Context    InboundContext `json:"context"`
	AgentID    string         `json:"agent_id,omitempty"`
	SessionKey string         `json:"session_key,omitempty"`
	Scope      *OutboundScope `json:"scope,omitempty"`
	Parts      []MediaPart    `json:"parts"`
}
