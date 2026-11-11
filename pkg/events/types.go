package events

import "time"

// Kind 标识运行时事件的类别。
type Kind string

// String 返回事件 kind 的字符串表示。
func (k Kind) String() string {
	return string(k)
}

// Event 是 Chagent 各组件共享的运行时事件信封。
type Event struct {
	ID          string         `json:"id"`
	Kind        Kind           `json:"kind"`
	Time        time.Time      `json:"time"`
	Source      Source         `json:"source"`
	Scope       Scope          `json:"scope,omitempty"`
	Correlation Correlation    `json:"correlation,omitempty"`
	Severity    Severity       `json:"severity,omitempty"`
	Payload     any            `json:"payload,omitempty"`
	Attrs       map[string]any `json:"attrs,omitempty"`
}

// Source 标识发出事件的组件。
type Source struct {
	Component string `json:"component"`
	Name      string `json:"name,omitempty"`
}

// Scope 标识事件的运行时归属。
//
// Scope 有意限定为 agent、session、turn、channel、chat、
// message 与 sender 身份。工具、provider、模型与 MCP 细节
// 应放在 Source、Payload 或 Attrs 中。
type Scope struct {
	RuntimeID string `json:"runtime_id,omitempty"`

	AgentID    string `json:"agent_id,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`

	Channel string `json:"channel,omitempty"`
	Account string `json:"account,omitempty"`
	ChatID  string `json:"chat_id,omitempty"`
	TopicID string `json:"topic_id,omitempty"`

	SpaceID   string `json:"space_id,omitempty"`
	SpaceType string `json:"space_type,omitempty"`
	ChatType  string `json:"chat_type,omitempty"`

	SenderID  string `json:"sender_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// Correlation 携带跨事件追踪字段。
type Correlation struct {
	TraceID      string `json:"trace_id,omitempty"`
	ParentTurnID string `json:"parent_turn_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	ReplyToID    string `json:"reply_to_id,omitempty"`
}

// Severity 描述事件的操作严重级别。
type Severity string

const (
	// SeverityDebug 用于冗长的诊断事件。
	SeverityDebug Severity = "debug"
	// SeverityInfo 用于正常的生命周期与活动事件。
	SeverityInfo Severity = "info"
	// SeverityWarn 用于可恢复的异常事件。
	SeverityWarn Severity = "warn"
	// SeverityError 用于失败的操作与不可恢复的事件。
	SeverityError Severity = "error"
)
