package messageutil

import (
	"strings"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// IsTransientAssistantThoughtMessage 报告 msg 是否是一条无效的、仅含推理内容的
// assistant 历史记录。这些“悬空”的思考消息不是规范的持久化格式，
// 应当丢弃而不是重放或重建。
func IsTransientAssistantThoughtMessage(msg protocoltypes.Message) bool {
	return msg.Role == "assistant" &&
		strings.TrimSpace(msg.Content) == "" &&
		strings.TrimSpace(msg.ReasoningContent) != "" &&
		len(msg.ToolCalls) == 0 &&
		len(msg.Media) == 0 &&
		len(msg.Attachments) == 0 &&
		strings.TrimSpace(msg.ToolCallID) == ""
}

// FilterInvalidHistoryMessages 移除无效的持久化历史记录，例如
// 临时的、仅含思考内容的 assistant 消息。
func FilterInvalidHistoryMessages(history []protocoltypes.Message) []protocoltypes.Message {
	if len(history) == 0 {
		return []protocoltypes.Message{}
	}

	filtered := make([]protocoltypes.Message, 0, len(history))
	for _, msg := range history {
		if IsTransientAssistantThoughtMessage(msg) {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}
