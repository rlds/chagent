package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// ErrBusClosed 在向已关闭的 MessageBus 发布消息时返回。
var ErrBusClosed = errors.New("message bus closed")

// ErrBusBackpressure 在发布尝试超出配置的背压等待预算并且消息被丢弃时返回。
var ErrBusBackpressure = errors.New("message bus backpressure")

var (
	ErrMissingInboundContext       = errors.New("inbound message context is required")
	ErrMissingOutboundContext      = errors.New("outbound message context is required")
	ErrMissingOutboundMediaContext = errors.New("outbound media context is required")
)

const defaultBusBufferSize = 64

const ()

type publishPolicy struct {
	stream string
	// timeout 是背压丢弃预算。为正值时，若通道已满，消息会在该时长后
	// 被丢弃。为零时，发布操作会阻塞，直到上下文取消或总线关闭
	//（不丢弃）。
	timeout time.Duration
}

type streamStats struct {
	dropped       atomic.Uint64
	lastDropped   atomic.Int64
	lastWaitNanos atomic.Int64
}

type MessageBusStats struct {
	Inbound       StreamStats `json:"inbound"`
	Outbound      StreamStats `json:"outbound"`
	OutboundMedia StreamStats `json:"outbound_media"`
}

type StreamStats struct {
	Depth              int       `json:"depth"`
	Capacity           int       `json:"capacity"`
	DroppedTotal       uint64    `json:"dropped_total"`
	LastDroppedAt      time.Time `json:"last_dropped_at,omitempty"`
	LastDropWait       string    `json:"last_drop_wait,omitempty"`
	LastDropWaitMillis int64     `json:"last_drop_wait_ms,omitempty"`
}

// StreamDelegate 由 channel Manager 实现，用于向 agent loop 提供流式能力，
// 同时避免紧密耦合。
type StreamDelegate interface {
	// GetStreamer 在通道支持流式输出时，为给定的 channel+chatID 返回 Streamer。
	// 若流式能力不可用，则返回 nil, false。
	GetStreamer(ctx context.Context, channel, chatID, sessionKey string) (Streamer, bool)
}

// Streamer 向支持流式输出的通道推送增量内容。
// 在此定义是为了让 agent loop 无需导入 pkg/channels 即可使用。
type Streamer interface {
	Update(ctx context.Context, content string) error
	Finalize(ctx context.Context, content string) error
	Cancel(ctx context.Context)
}

// ContextUsageStreamer 可在流式通道的最终消息替代常规出站响应时，
// 附加最终的上下文窗口用量元数据。
type ContextUsageStreamer interface {
	Streamer
	FinalizeWithContext(ctx context.Context, content string, usage *ContextUsage) error
}

// LLMUsageStreamer 可在流式响应结束时附加 LLM API 用量元数据
// （提示词 token、补全 token、缓存命中信息等）。
type LLMUsageStreamer interface {
	Streamer
	SetUsage(usage *protocoltypes.UsageInfo)
}

// ReasoningStreamer 可将模型逐步推理/思考内容与最终面向用户的答案流
// 分开显示。
type ReasoningStreamer interface {
	UpdateReasoning(ctx context.Context, content string) error
	FinalizeReasoning(ctx context.Context, content string) error
}

type MessageBus struct {
	inbound       chan InboundMessage
	outbound      chan OutboundMessage
	outboundMedia chan OutboundMediaMessage

	closeOnce      sync.Once
	done           chan struct{}
	closed         atomic.Bool
	wg             sync.WaitGroup
	publishMu      sync.Mutex
	streamDelegate atomic.Value // 存储 StreamDelegate
	eventPublisher atomic.Value // 存储 EventPublisher
	inboundStats   streamStats
	outboundStats  streamStats
	mediaStats     streamStats
}

// EventPublisher 是 MessageBus 使用的最小运行时事件发布器。
type EventPublisher interface {
	Publish(ctx context.Context, evt runtimeevents.Event) runtimeevents.PublishResult
	PublishNonBlocking(evt runtimeevents.Event) runtimeevents.PublishResult
}

func NewMessageBus() *MessageBus {
	return &MessageBus{
		inbound:       make(chan InboundMessage, defaultBusBufferSize),
		outbound:      make(chan OutboundMessage, defaultBusBufferSize),
		outboundMedia: make(chan OutboundMediaMessage, defaultBusBufferSize),
		done:          make(chan struct{}),
	}
}

func (mb *MessageBus) enterPublish(ctx context.Context) error {
	mb.publishMu.Lock()
	defer mb.publishMu.Unlock()

	if mb.closed.Load() {
		return ErrBusClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-mb.done:
		return ErrBusClosed
	default:
	}

	mb.wg.Add(1)
	return nil
}

func publish[T any](
	ctx context.Context,
	mb *MessageBus,
	ch chan T,
	msg T,
	policy publishPolicy,
	stats *streamStats,
	scope runtimeevents.Scope,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := mb.enterPublish(ctx); err != nil {
		return err
	}
	defer mb.wg.Done()

	// timeout == 0 表示没有背压丢弃预算；此时会阻塞，直到上下文取消
	// 或总线关闭。这是关键流（inbound、outbound、outboundMedia、voiceControl）
	// 的默认行为，因为静默丢弃这些消息是不可取的。
	if policy.timeout > 0 {
		timer := time.NewTimer(policy.timeout)
		defer timer.Stop()

		select {
		case ch <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			droppedTotal := stats.dropped.Add(1)
			now := time.Now()
			stats.lastDropped.Store(now.UnixNano())
			stats.lastWaitNanos.Store(policy.timeout.Nanoseconds())
			queueDepth := len(ch)
			queueCap := cap(ch)
			mb.publishDrop(
				policy.stream, scope, "queue_full_timeout",
				policy.timeout, queueDepth, queueCap, droppedTotal,
			)
			logger.WarnCF("bus", "Dropped bus message due to backpressure", map[string]any{
				"stream":         policy.stream,
				"wait_ms":        policy.timeout.Milliseconds(),
				"queue_depth":    queueDepth,
				"queue_capacity": queueCap,
				"dropped_total":  droppedTotal,
			})
			return fmt.Errorf("%w: %s queue full after %s", ErrBusBackpressure, policy.stream, policy.timeout)
		case <-mb.done:
			return ErrBusClosed
		}
	}

	select {
	case ch <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-mb.done:
		return ErrBusClosed
	}
}

func (mb *MessageBus) PublishInbound(ctx context.Context, msg InboundMessage) error {
	msg = NormalizeInboundMessage(msg)
	if msg.Context.isZero() {
		mb.publishFailure("inbound", runtimeScopeFromInboundContext(msg.Context), ErrMissingInboundContext)
		return ErrMissingInboundContext
	}
	if err := publish(ctx, mb, mb.inbound, msg, publishPolicy{
		stream: "inbound",
	}, &mb.inboundStats, runtimeScopeFromInboundContext(msg.Context)); err != nil {
		scope := runtimeScopeFromInboundContext(msg.Context)
		if !errors.Is(err, ErrBusBackpressure) {
			mb.publishFailure("inbound", scope, err)
		}
		return err
	}
	return nil
}

func (mb *MessageBus) InboundChan() <-chan InboundMessage {
	return mb.inbound
}

func (mb *MessageBus) PublishOutbound(ctx context.Context, msg OutboundMessage) error {
	msg = NormalizeOutboundMessage(msg)
	if msg.Context.isZero() {
		mb.publishFailure("outbound", runtimeScopeFromInboundContext(msg.Context), ErrMissingOutboundContext)
		return ErrMissingOutboundContext
	}
	if err := publish(ctx, mb, mb.outbound, msg, publishPolicy{
		stream: "outbound",
	}, &mb.outboundStats, runtimeScopeFromInboundContext(msg.Context)); err != nil {
		scope := runtimeScopeFromInboundContext(msg.Context)
		if !errors.Is(err, ErrBusBackpressure) {
			mb.publishFailure("outbound", scope, err)
		}
		return err
	}
	return nil
}

func (mb *MessageBus) OutboundChan() <-chan OutboundMessage {
	return mb.outbound
}

func (mb *MessageBus) PublishOutboundMedia(ctx context.Context, msg OutboundMediaMessage) error {
	msg = NormalizeOutboundMediaMessage(msg)
	if msg.Context.isZero() {
		mb.publishFailure("outbound_media", runtimeScopeFromInboundContext(msg.Context), ErrMissingOutboundMediaContext)
		return ErrMissingOutboundMediaContext
	}
	if err := publish(ctx, mb, mb.outboundMedia, msg, publishPolicy{
		stream: "outbound_media",
	}, &mb.mediaStats, runtimeScopeFromInboundContext(msg.Context)); err != nil {
		scope := runtimeScopeFromInboundContext(msg.Context)
		if !errors.Is(err, ErrBusBackpressure) {
			mb.publishFailure("outbound_media", scope, err)
		}
		return err
	}
	return nil
}

func (mb *MessageBus) OutboundMediaChan() <-chan OutboundMediaMessage {
	return mb.outboundMedia
}

// SetStreamDelegate 注册一个 StreamDelegate（通常是 channel Manager）。
func (mb *MessageBus) SetStreamDelegate(d StreamDelegate) {
	mb.streamDelegate.Store(d)
}

// SetEventPublisher 为总线错误和生命周期事件注册运行时事件发布器。
func (mb *MessageBus) SetEventPublisher(p EventPublisher) {
	mb.eventPublisher.Store(p)
}

// GetStreamer 通过委托方为给定的 channel+chatID+session 返回 Streamer。
func (mb *MessageBus) GetStreamer(ctx context.Context, channel, chatID, sessionKey string) (Streamer, bool) {
	if d, ok := mb.streamDelegate.Load().(StreamDelegate); ok && d != nil {
		return d.GetStreamer(ctx, channel, chatID, sessionKey)
	}
	return nil, false
}

func (mb *MessageBus) Stats() MessageBusStats {
	if mb == nil {
		return MessageBusStats{}
	}
	return MessageBusStats{
		Inbound:       snapshotStreamStats(mb.inbound, &mb.inboundStats),
		Outbound:      snapshotStreamStats(mb.outbound, &mb.outboundStats),
		OutboundMedia: snapshotStreamStats(mb.outboundMedia, &mb.mediaStats),
	}
}

// HealthCheck 返回所有流的队列深度与累计丢弃次数的快照。
// 它始终报告 ok=true：由背压导致的丢弃会体现在 message 字符串中用于遥测，
// 但不影响该布尔值。需要就绪语义（例如发生丢弃时返回 503）的调用方
// 应直接检查 Stats() 并应用自己的阈值逻辑。
func (mb *MessageBus) HealthCheck() (bool, string) {
	stats := mb.Stats()
	totalDropped := stats.Inbound.DroppedTotal +
		stats.Outbound.DroppedTotal +
		stats.OutboundMedia.DroppedTotal
	message := fmt.Sprintf(
		"in=%d/%d out=%d/%d media=%d/%d dropped=%d",
		stats.Inbound.Depth,
		stats.Inbound.Capacity,
		stats.Outbound.Depth,
		stats.Outbound.Capacity,
		stats.OutboundMedia.Depth,
		stats.OutboundMedia.Capacity,
		totalDropped,
	)

	return true, message
}

func snapshotStreamStats[T any](ch chan T, stats *streamStats) StreamStats {
	snapshot := StreamStats{
		DroppedTotal: stats.dropped.Load(),
	}
	if ch != nil {
		snapshot.Depth = len(ch)
		snapshot.Capacity = cap(ch)
	}
	if unixNano := stats.lastDropped.Load(); unixNano > 0 {
		snapshot.LastDroppedAt = time.Unix(0, unixNano)
	}
	if waitNanos := stats.lastWaitNanos.Load(); waitNanos > 0 {
		wait := time.Duration(waitNanos)
		snapshot.LastDropWait = wait.String()
		snapshot.LastDropWaitMillis = wait.Milliseconds()
	}
	return snapshot
}

func (mb *MessageBus) Close() {
	mb.closeOnce.Do(func() {
		mb.publishCloseEvent(runtimeevents.KindBusCloseStarted, 0)

		mb.publishMu.Lock()
		mb.closed.Store(true)
		close(mb.done)
		mb.publishMu.Unlock()

		// 等待所有正在进行的 Publish 调用结束，确保所有消息都已发送到通道或已退出
		mb.wg.Wait()

		// 安全关闭通道
		close(mb.inbound)
		close(mb.outbound)
		close(mb.outboundMedia)

		// 清理通道中残留的消息
		drained := 0
		for range mb.inbound {
			drained++
		}
		for range mb.outbound {
			drained++
		}
		for range mb.outboundMedia {
			drained++
		}

		if drained > 0 {
			logger.DebugCF("bus", "Drained buffered messages during close", map[string]any{
				"count": drained,
			})
			mb.publishCloseEvent(runtimeevents.KindBusCloseDrained, drained)
		}
		mb.publishCloseEvent(runtimeevents.KindBusCloseCompleted, drained)
	})
}
