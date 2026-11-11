// Chagent - 超轻量级个人 AI agent

package adapters

import (
	"context"

	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
)

// channelManagerAdapter 包装 *channels.Manager，使其实现 interfaces.ChannelManager 接口。
type channelManagerAdapter struct {
	inner *channels.Manager
}

// NewChannelManager 为 *channels.Manager 创建适配器。
func NewChannelManager(inner *channels.Manager) interfaces.ChannelManager {
	return &channelManagerAdapter{inner: inner}
}

func (a *channelManagerAdapter) GetChannel(name string) (channels.Channel, bool) {
	return a.inner.GetChannel(name)
}

func (a *channelManagerAdapter) GetEnabledChannels() []string {
	return a.inner.GetEnabledChannels()
}

func (a *channelManagerAdapter) InvokeTypingStop(channel, chatID string) {
	a.inner.InvokeTypingStop(channel, chatID)
}

func (a *channelManagerAdapter) SendMessage(ctx context.Context, msg bus.OutboundMessage) error {
	return a.inner.SendMessage(ctx, msg)
}

func (a *channelManagerAdapter) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	return a.inner.SendMedia(ctx, msg)
}

func (a *channelManagerAdapter) SendPlaceholder(ctx context.Context, channel, chatID string) bool {
	return a.inner.SendPlaceholder(ctx, channel, chatID)
}

func (a *channelManagerAdapter) DismissToolFeedback(
	ctx context.Context, channel, chatID string, outboundCtx *bus.InboundContext,
) {
	a.inner.DismissToolFeedback(ctx, channel, chatID, outboundCtx)
}
