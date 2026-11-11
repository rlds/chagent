// Chagent - 超轻量级个人 AI 智能体

package interfaces

import (
	"context"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
)

// MessageBus 负责发布入站与出站消息。
// 它是 agent 循环的主要通信通道。
type MessageBus interface {
	// PublishInbound 发送一条待处理的入站消息。
	PublishInbound(ctx context.Context, msg bus.InboundMessage) error

	// PublishOutbound 将出站消息发送到对应的通道。
	PublishOutbound(ctx context.Context, msg bus.OutboundMessage) error

	// PublishOutboundMedia 发送一条出站媒体消息。
	PublishOutboundMedia(ctx context.Context, msg bus.OutboundMediaMessage) error

	// GetStreamer 在当前通道支持流式传输时返回该通道的 streamer。
	GetStreamer(ctx context.Context, channel, chatID, sessionKey string) (bus.Streamer, bool)

	// InboundChan 返回用于接收入站消息的通道。
	InboundChan() <-chan bus.InboundMessage
}

// ChannelManager 管理通道生命周期并提供通道访问能力。
type ChannelManager interface {
	// GetChannel 返回指定名称的通道。
	GetChannel(name string) (channels.Channel, bool)

	// GetEnabledChannels 返回已启用通道的名称列表。
	GetEnabledChannels() []string

	// InvokeTypingStop 发出输入状态已停止的信号。
	InvokeTypingStop(channel, chatID string)

	// SendMessage 向指定通道和会话发送文本消息。
	SendMessage(ctx context.Context, msg bus.OutboundMessage) error

	// SendMedia 向指定通道和会话发送媒体消息。
	SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error

	// SendPlaceholder 发送占位消息（例如用于音频转写）。
	SendPlaceholder(ctx context.Context, channel, chatID string) bool

	// DismissToolFeedback 清除指定通道/会话中已跟踪的工具反馈动画。
	// 当一轮对话在没有最终回复的情况下结束（例如 ResponseHandled 工具）时
	// 调用它，以避免动画 goroutine 成为孤儿。
	// outboundCtx 携带使用作用域跟踪键的通道（例如 Telegram 论坛主题）所需的
	// 话题/线程信息；可以为 nil。
	DismissToolFeedback(ctx context.Context, channel, chatID string, outboundCtx *bus.InboundContext)
}
