package channels

import (
	"context"

	"github.com/rlds/chagent/pkg/bus"
)

// MediaSender 是一个可选接口，面向能够发送媒体附件
// （图片、文件、音频、视频）的通道。
// Manager 通过类型断言发现实现此接口的通道，
// 并将 OutboundMediaMessage 路由给它们。
type MediaSender interface {
	SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error)
}
