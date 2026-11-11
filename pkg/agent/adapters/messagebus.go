// Chagent - 超轻量个人 AI 智能体

package adapters

import (
	"context"

	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/bus"
)

// messageBusAdapter 包装 *bus.MessageBus，以实现 interfaces.MessageBus。
type messageBusAdapter struct {
	inner *bus.MessageBus
}

// NewMessageBus 为 *bus.MessageBus 创建一个适配器。
func NewMessageBus(inner *bus.MessageBus) interfaces.MessageBus {
	return &messageBusAdapter{inner: inner}
}

func (a *messageBusAdapter) PublishInbound(ctx context.Context, msg bus.InboundMessage) error {
	return a.inner.PublishInbound(ctx, msg)
}

func (a *messageBusAdapter) PublishOutbound(ctx context.Context, msg bus.OutboundMessage) error {
	return a.inner.PublishOutbound(ctx, msg)
}

func (a *messageBusAdapter) PublishOutboundMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	return a.inner.PublishOutboundMedia(ctx, msg)
}

func (a *messageBusAdapter) GetStreamer(ctx context.Context, channel, chatID, sessionKey string) (bus.Streamer, bool) {
	return a.inner.GetStreamer(ctx, channel, chatID, sessionKey)
}

func (a *messageBusAdapter) InboundChan() <-chan bus.InboundMessage {
	return a.inner.InboundChan()
}
