package bus

import "strings"

// NewOutboundContext 构建投递出站文本消息或回复所需的最小规范化寻址上下文。
func NewOutboundContext(channel, chatID, replyToMessageID string) InboundContext {
	return normalizeInboundContext(InboundContext{
		Channel:          strings.TrimSpace(channel),
		ChatID:           strings.TrimSpace(chatID),
		ReplyToMessageID: strings.TrimSpace(replyToMessageID),
	})
}

// NormalizeOutboundMessage 确保 Context 已被规范化，并为运行时消费者
// 同步便捷镜像字段。
func NormalizeOutboundMessage(msg OutboundMessage) OutboundMessage {
	msg.Channel = strings.TrimSpace(msg.Channel)
	msg.ChatID = strings.TrimSpace(msg.ChatID)
	msg.ReplyToMessageID = strings.TrimSpace(msg.ReplyToMessageID)
	if msg.Context.Channel == "" {
		msg.Context.Channel = msg.Channel
	}
	if msg.Context.ChatID == "" {
		msg.Context.ChatID = msg.ChatID
	}
	if msg.Context.ReplyToMessageID == "" {
		msg.Context.ReplyToMessageID = msg.ReplyToMessageID
	}
	msg.Context = normalizeInboundContext(msg.Context)
	if msg.Channel == "" {
		msg.Channel = msg.Context.Channel
	}
	if msg.ChatID == "" {
		msg.ChatID = msg.Context.ChatID
	}
	if msg.ReplyToMessageID == "" {
		msg.ReplyToMessageID = msg.Context.ReplyToMessageID
	}
	if msg.Context.ReplyToMessageID == "" {
		msg.Context.ReplyToMessageID = msg.ReplyToMessageID
	}
	msg.Scope = cloneOutboundScope(msg.Scope)
	return msg
}

// NormalizeOutboundMediaMessage 确保媒体出站消息同样携带规范化上下文，
// 并保持便捷镜像字段同步。
func NormalizeOutboundMediaMessage(msg OutboundMediaMessage) OutboundMediaMessage {
	msg.Channel = strings.TrimSpace(msg.Channel)
	msg.ChatID = strings.TrimSpace(msg.ChatID)
	if msg.Context.Channel == "" {
		msg.Context.Channel = msg.Channel
	}
	if msg.Context.ChatID == "" {
		msg.Context.ChatID = msg.ChatID
	}
	msg.Context = normalizeInboundContext(msg.Context)
	if msg.Channel == "" {
		msg.Channel = msg.Context.Channel
	}
	if msg.ChatID == "" {
		msg.ChatID = msg.Context.ChatID
	}
	msg.Scope = cloneOutboundScope(msg.Scope)
	return msg
}

func cloneOutboundScope(scope *OutboundScope) *OutboundScope {
	if scope == nil {
		return nil
	}
	cloned := *scope
	if len(scope.Dimensions) > 0 {
		cloned.Dimensions = append([]string(nil), scope.Dimensions...)
	}
	if len(scope.Values) > 0 {
		cloned.Values = make(map[string]string, len(scope.Values))
		for key, value := range scope.Values {
			cloned.Values[key] = value
		}
	}
	return &cloned
}
