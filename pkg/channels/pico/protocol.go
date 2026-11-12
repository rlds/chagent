package pico

import (
	"strings"
	"time"
)

// 协议消息类型。
const (
	// TypeMessageSend 由客户端发往服务端。
	TypeMessageSend = "message.send"
	TypeMediaSend   = "media.send"
	TypePing        = "ping"

	// TypeMessageCreate 由服务端发往客户端。
	TypeMessageCreate = "message.create"
	TypeMessageUpdate = "message.update"
	TypeMessageDelete = "message.delete"
	TypeMediaCreate   = "media.create"
	TypeTypingStart   = "typing.start"
	TypeTypingStop    = "typing.stop"
	TypeError         = "error"
	TypePong          = "pong"

	// TypeExecutionReport 由 chagent 发往 pico 对端：一次工具执行的完成上报。
	// 它与 message.* 并列，不是聊天消息：不进入会话历史，也不参与消息编辑/删除。
	TypeExecutionReport = "execution.report"

	PayloadKeyContent      = "content"
	PayloadKeyThought      = "thought"
	PayloadKeyKind         = "kind"
	PayloadKeyPlaceholder  = "placeholder"
	PayloadKeyToolCalls    = "tool_calls"
	PayloadKeyModelName    = "model_name"
	PayloadKeyUsage        = "usage"         // LLM API 使用量信息（提示/补全/总计 + 缓存命中）
	PayloadKeyContextUsage = "context_usage" // 上下文窗口使用量估算

	MessageKindThought   = "thought"
	MessageKindToolCalls = "tool_calls"

	// MessageKindExecutionReport 是 execution.report 载荷的 kind 值，
	// 供同时消费 message.* 与 execution.report 的对端统一分流。
	MessageKindExecutionReport = "execution_report"
)

// PicoMessage 是所有 Pico 协议消息的线上传输格式。
type PicoMessage struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Timestamp int64          `json:"timestamp,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// newMessage 使用给定的类型和载荷创建一个 PicoMessage。
func newMessage(msgType string, payload map[string]any) PicoMessage {
	return PicoMessage{
		Type:      msgType,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}

func isThoughtPayload(payload map[string]any) bool {
	kind, _ := payload[PayloadKeyKind].(string)
	if strings.EqualFold(strings.TrimSpace(kind), MessageKindThought) {
		return true
	}

	// 保持 pico_client 对仍然发送 kind 出现前布尔思考标记的
	// 旧版服务端的入站兼容性。
	thought, _ := payload[PayloadKeyThought].(bool)
	return thought
}

func newErrorWithPayload(code, message string, extra map[string]any) PicoMessage {
	payload := map[string]any{
		"code":    code,
		"message": message,
	}
	for key, value := range extra {
		payload[key] = value
	}
	return newMessage(TypeError, payload)
}

// newError 创建一个错误类型的 PicoMessage。
func newError(code, message string) PicoMessage {
	return newErrorWithPayload(code, message, nil)
}
