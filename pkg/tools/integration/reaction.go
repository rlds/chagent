package integrationtools

import (
	"context"
	"fmt"
)

type ReactionCallback func(ctx context.Context, channel, chatID, messageID string) error

type ReactionTool struct {
	reactionCallback ReactionCallback
}

func NewReactionTool() *ReactionTool {
	return &ReactionTool{}
}

func (t *ReactionTool) Name() string {
	return "reaction"
}

func (t *ReactionTool) Description() string {
	return "给消息添加表情回应。省略 message_id 时默认使用当前入站消息。"
}

func (t *ReactionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{
				"type":        "string",
				"description": "可选：目标消息 ID；默认当前入站消息",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "可选：目标通道（telegram、whatsapp 等）",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "可选：目标聊天/用户 ID",
			},
		},
	}
}

func (t *ReactionTool) SetReactionCallback(callback ReactionCallback) {
	t.reactionCallback = callback
}

func (t *ReactionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	messageID, _ := args["message_id"].(string)

	if channel == "" {
		channel = ToolChannel(ctx)
	}
	if chatID == "" {
		chatID = ToolChatID(ctx)
	}
	if messageID == "" {
		messageID = ToolMessageID(ctx)
	}

	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "未指定目标 channel/chat", IsError: true}
	}
	if messageID == "" {
		return &ToolResult{ForLLM: "message_id 为必填项", IsError: true}
	}
	if t.reactionCallback == nil {
		return &ToolResult{ForLLM: "表情回应未配置", IsError: true}
	}

	if err := t.reactionCallback(ctx, channel, chatID, messageID); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("添加表情回应失败：%v", err),
			IsError: true,
			Err:     err,
		}
	}

	return &ToolResult{
		ForLLM: fmt.Sprintf("已对 %s:%s 的消息 %s 添加表情回应", channel, chatID, messageID),
		Silent: true,
	}
}
