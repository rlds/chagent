// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package channels

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/constants"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/health"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/utils"
)

const (
	defaultChannelQueueSize = 16
	defaultRateLimit        = 10 // 默认 10 msg/s
	maxRetries              = 3
	rateLimitDelay          = 1 * time.Second
	baseBackoff             = 500 * time.Millisecond
	maxBackoff              = 8 * time.Second

	janitorInterval = 10 * time.Second
	typingStopTTL   = 5 * time.Minute
	placeholderTTL  = 10 * time.Minute

	streamAuxiliaryTombstoneTTL = 30 * time.Second
)

// typingEntry 将 typing 停止函数与创建时间戳封装在一起，用于按 TTL 淘汰。
type typingEntry struct {
	stop      func()
	createdAt time.Time
}

// reactionEntry 将 reaction 撤销函数与创建时间戳封装在一起，用于按 TTL 淘汰。
type reactionEntry struct {
	undo      func()
	createdAt time.Time
}

// placeholderEntry 将占位符 ID 与创建时间戳封装在一起，用于按 TTL 淘汰。
type placeholderEntry struct {
	id        string
	createdAt time.Time
}

// channelRateConfig 将 channel 名称映射到每秒速率限制。
var channelRateConfig = map[string]float64{
	"telegram": 20,
	"discord":  1,
	"slack":    1,
	"matrix":   2,
	"line":     10,
	"qq":       5,
	"irc":      2,
}

type channelWorker struct {
	ch         Channel
	queue      chan bus.OutboundMessage
	mediaQueue chan bus.OutboundMediaMessage
	done       chan struct{}
	mediaDone  chan struct{}
	limiter    *rate.Limiter
}

type Manager struct {
	channels      map[string]Channel
	workers       map[string]*channelWorker
	bus           *bus.MessageBus
	runtimeEvents runtimeevents.Bus
	config        *config.Config
	mediaStore    media.MediaStore
	dispatchTask  *asyncTask
	// dispatchCtx 是 StartAll 启动 dispatcher/worker 时使用的常驻上下文。
	// Reload 必须复用它：dispatcher 生命周期归 StartAll/StopAll 管理，
	// 否则 StopAll 取消的会是 Reload 新建的孤儿 ctx，而旧 dispatcher 永不退出。
	dispatchCtx context.Context
	// dispatchWG 跟踪 StartAll 启动的长运行 dispatcher goroutine，
	// 使 StopAll 在关闭 worker 队列前能 Join 它们。
	dispatchWG                sync.WaitGroup
	mux                       *dynamicServeMux
	httpServer                *http.Server
	httpListeners             []net.Listener
	mu                        sync.RWMutex
	placeholders              sync.Map          // "channel:chatID" → placeholderID（字符串）
	typingStops               sync.Map          // "channel:chatID" → func()
	reactionUndos             sync.Map          // "channel:chatID" → reactionEntry
	streamActive              sync.Map          // streamSuppressionKey → true（streamer.Finalize 发送该消息时置位）
	streamAuxiliaryTombstones sync.Map          // streamSuppressionKey → time.Time（丢弃 stream 结束后迟到的辅助消息）
	channelHashes             map[string]string // channel 名称 → 配置哈希
}

type mediaStoreSetter interface {
	SetMediaStore(s media.MediaStore)
}

// ManagerOption 配置 channel Manager。
type ManagerOption func(*Manager)

// WithRuntimeEvents 注入用于 channel 观测的运行时事件总线。
func WithRuntimeEvents(eventBus runtimeevents.Bus) ManagerOption {
	return func(m *Manager) {
		m.runtimeEvents = eventBus
	}
}

// ChannelLifecyclePayload 描述 channel 生命周期的运行时事件。
type ChannelLifecyclePayload struct {
	Type  string `json:"type,omitempty"`
	Error string `json:"error,omitempty"`
}

// ChannelOutboundPayload 描述 channel 出站消息的运行时事件。
type ChannelOutboundPayload struct {
	Media            bool     `json:"media,omitempty"`
	ContentLen       int      `json:"content_len,omitempty"`
	MessageIDs       []string `json:"message_ids,omitempty"`
	ReplyToMessageID string   `json:"reply_to_message_id,omitempty"`
	Error            string   `json:"error,omitempty"`
	Retries          int      `json:"retries,omitempty"`
}

type toolFeedbackMessageTracker interface {
	RecordToolFeedbackMessage(chatID, messageID, content string)
	ClearToolFeedbackMessage(chatID string)
}

type toolFeedbackMessageCleaner interface {
	DismissToolFeedbackMessage(ctx context.Context, chatID string)
}

type toolFeedbackMessageTargetResolver interface {
	ToolFeedbackMessageChatID(chatID string, outboundCtx *bus.InboundContext) string
}

type toolFeedbackMessageContentPreparer interface {
	PrepareToolFeedbackMessageContent(content string) string
}

type asyncTask struct {
	cancel context.CancelFunc
}

func outboundMessageChannel(msg bus.OutboundMessage) string {
	return msg.Context.Channel
}

func outboundMessageChatID(msg bus.OutboundMessage) string {
	return msg.ChatID
}

func outboundMessageIsToolFeedback(msg bus.OutboundMessage) bool {
	if len(msg.Context.Raw) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["message_kind"]), "tool_feedback")
}

func outboundMessageIsToolCalls(msg bus.OutboundMessage) bool {
	if len(msg.Context.Raw) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["message_kind"]), "tool_calls")
}

func outboundMessageHasAuxiliaryKind(msg bus.OutboundMessage) bool {
	if len(msg.Context.Raw) == 0 {
		return false
	}
	return strings.TrimSpace(msg.Context.Raw["message_kind"]) != ""
}

func outboundMessageIsFinal(msg bus.OutboundMessage) bool {
	if len(msg.Context.Raw) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["outbound_kind"]), "final")
}

func outboundMessageBypassesPlaceholderEdit(msg bus.OutboundMessage) bool {
	if len(msg.Context.Raw) == 0 {
		return false
	}
	kind := strings.TrimSpace(msg.Context.Raw["message_kind"])
	return strings.EqualFold(kind, "thought") || strings.EqualFold(kind, "tool_calls")
}

func outboundMessageEditPayload(msg bus.OutboundMessage, content string) map[string]any {
	payload := map[string]any{
		"content": content,
	}
	if len(msg.Context.Raw) == 0 {
		return payload
	}
	if modelName := strings.TrimSpace(msg.Context.Raw["model_name"]); modelName != "" {
		payload["model_name"] = modelName
	}
	return payload
}

func outboundMediaChannel(msg bus.OutboundMediaMessage) string {
	return msg.Context.Channel
}

func outboundMediaChatID(msg bus.OutboundMediaMessage) string {
	return msg.ChatID
}

func streamSuppressionKey(channel, chatID, sessionKey string) string {
	key := channel + ":" + chatID
	if strings.TrimSpace(sessionKey) == "" {
		return key
	}
	return key + ":" + sessionKey
}

func trackedToolFeedbackMessageChatID(ch Channel, chatID string, outboundCtx *bus.InboundContext) string {
	if resolver, ok := ch.(toolFeedbackMessageTargetResolver); ok {
		if resolved := strings.TrimSpace(resolver.ToolFeedbackMessageChatID(chatID, outboundCtx)); resolved != "" {
			return resolved
		}
	}
	return strings.TrimSpace(chatID)
}

func dismissTrackedToolFeedbackMessage(
	ctx context.Context,
	ch Channel,
	chatID string,
	outboundCtx *bus.InboundContext,
) {
	trackedChatID := trackedToolFeedbackMessageChatID(ch, chatID, outboundCtx)
	if trackedChatID == "" {
		return
	}
	if cleaner, ok := ch.(toolFeedbackMessageCleaner); ok {
		cleaner.DismissToolFeedbackMessage(ctx, trackedChatID)
		return
	}
	if tracker, ok := ch.(toolFeedbackMessageTracker); ok {
		tracker.ClearToolFeedbackMessage(trackedChatID)
	}
}

func clearTrackedToolFeedbackMessage(
	ch Channel,
	chatID string,
	outboundCtx *bus.InboundContext,
) {
	trackedChatID := trackedToolFeedbackMessageChatID(ch, chatID, outboundCtx)
	if trackedChatID == "" {
		return
	}
	if tracker, ok := ch.(toolFeedbackMessageTracker); ok {
		tracker.ClearToolFeedbackMessage(trackedChatID)
	}
}

// DismissToolFeedback 清除指定 channel/chat 上已跟踪的 tool feedback 动画。
// 当一轮对话结束时没有最终响应（例如 ResponseHandled 工具）就会调用它，
// 以停止遗留的动画 goroutine。
// outboundCtx 携带 topic/thread 信息，供使用作用域跟踪键的 channel
// （例如 Telegram 论坛主题）使用；对无主题的 channel 可为 nil。
func (m *Manager) DismissToolFeedback(
	ctx context.Context, channelName, chatID string, outboundCtx *bus.InboundContext,
) {
	ch, ok := m.GetChannel(channelName)
	if !ok {
		return
	}
	dismissTrackedToolFeedbackMessage(ctx, ch, chatID, outboundCtx)
}

func prepareToolFeedbackMessageContent(ch Channel, content string) string {
	prepared := strings.TrimSpace(content)
	if prepared == "" {
		return ""
	}
	if preparer, ok := ch.(toolFeedbackMessageContentPreparer); ok {
		if candidate := strings.TrimSpace(preparer.PrepareToolFeedbackMessageContent(prepared)); candidate != "" {
			return candidate
		}
	}
	return prepared
}

func (m *Manager) toolFeedbackSeparateMessagesEnabled() bool {
	if m == nil || m.config == nil {
		return false
	}
	return m.config.Agents.Defaults.IsToolFeedbackSeparateMessagesEnabled()
}

// RecordPlaceholder 注册一条占位符消息，供之后编辑。
// 实现 PlaceholderRecorder。
func (m *Manager) RecordPlaceholder(channel, chatID, placeholderID string) {
	key := channel + ":" + chatID
	m.placeholders.Store(key, placeholderEntry{id: placeholderID, createdAt: time.Now()})
}

// SendPlaceholder 为指定的 channel/chatID 发送 "Thinking…" 占位符，
// 并记录下来供之后编辑。若已发送占位符则返回 true。
func (m *Manager) SendPlaceholder(ctx context.Context, channel, chatID string) bool {
	m.mu.RLock()
	ch, ok := m.channels[channel]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	pc, ok := ch.(PlaceholderCapable)
	if !ok {
		return false
	}
	phID, err := pc.SendPlaceholder(ctx, chatID)
	if err != nil || phID == "" {
		return false
	}
	m.RecordPlaceholder(channel, chatID, phID)
	return true
}

// RecordTypingStop 注册一个 typing 停止函数，供之后调用。
// 实现 PlaceholderRecorder。
func (m *Manager) RecordTypingStop(channel, chatID string, stop func()) {
	key := channel + ":" + chatID
	entry := typingEntry{stop: stop, createdAt: time.Now()}
	if previous, loaded := m.typingStops.Swap(key, entry); loaded {
		if oldEntry, ok := previous.(typingEntry); ok && oldEntry.stop != nil {
			oldEntry.stop()
		}
	}
}

// InvokeTypingStop 调用为指定 channel 和 chatID 注册的 typing 停止函数。
// 即使当前没有激活的 typing 指示器，调用也是安全的（空操作）。
// agent 循环在处理完成（成功、出错或 panic）时用它停止 typing，
// 无论是否发布了出站消息。
func (m *Manager) InvokeTypingStop(channel, chatID string) {
	key := channel + ":" + chatID
	if v, loaded := m.typingStops.LoadAndDelete(key); loaded {
		if entry, ok := v.(typingEntry); ok {
			entry.stop()
		}
	}
}

// RecordReactionUndo 注册一个 reaction 撤销函数，供之后调用。
// 实现 PlaceholderRecorder。
func (m *Manager) RecordReactionUndo(channel, chatID string, undo func()) {
	key := channel + ":" + chatID
	m.reactionUndos.Store(key, reactionEntry{undo: undo, createdAt: time.Now()})
}

// preSend 在发送消息前处理 typing 停止、reaction 撤销和占位符编辑。
// 若在常规 Send 之前已完成投递，则返回已投递的消息 ID 和 true。
func (m *Manager) preSend(ctx context.Context, name string, msg bus.OutboundMessage, ch Channel) ([]string, bool) {
	chatID := outboundMessageChatID(msg)
	key := name + ":" + chatID
	streamKey := streamSuppressionKey(name, chatID, msg.SessionKey)

	// 1. 停止 typing
	if v, loaded := m.typingStops.LoadAndDelete(key); loaded {
		if entry, ok := v.(typingEntry); ok {
			entry.stop() // 幂等，安全
		}
	}

	// 2. 撤销 reaction
	if v, loaded := m.reactionUndos.LoadAndDelete(key); loaded {
		if entry, ok := v.(reactionEntry); ok {
			entry.undo() // 幂等，安全
		}
	}

	isToolFeedback := outboundMessageIsToolFeedback(msg)
	isToolCalls := outboundMessageIsToolCalls(msg)
	isAuxiliaryMessage := outboundMessageHasAuxiliaryKind(msg)
	isFinalMessage := outboundMessageIsFinal(msg)
	separateToolFeedbackMessages := m.toolFeedbackSeparateMessagesEnabled()

	// 3. 如果某个 stream 已经为这个 chat 完成终结，则必须丢弃过时的辅助消息，
	// 且不能消耗 final 响应标记。stream 终结会绕过 worker 队列，因此排队中的
	// 旧 feedback/thoughts 可能在清理标记和占位符的常规 final 出站消息之前到达。
	// 注意：tool_calls 消息绝不能丢弃，因为它们代表当前轮次新的工具调用，
	// 必须投递给 UI。
	if isAuxiliaryMessage && !isToolCalls {
		if _, loaded := m.streamActive.Load(streamKey); loaded {
			return nil, true
		}
		if m.streamAuxiliaryTombstoneActive(streamKey) {
			return nil, true
		}
	}

	// 4. 如果某个 stream 已经完成本轮终结，只跳过重复的 final 出站消息。
	// 更早排队的可见消息仍必须投递。
	if isFinalMessage {
		if _, loaded := m.streamActive.LoadAndDelete(streamKey); loaded {
			if v, loaded := m.placeholders.LoadAndDelete(key); loaded {
				if entry, ok := v.(placeholderEntry); ok && entry.id != "" {
					// 优先删除占位符（比编辑成相同内容体验更干净）
					if deleter, ok := ch.(MessageDeleter); ok {
						deleter.DeleteMessage(ctx, chatID, entry.id) // 尽力而为
					} else if editor, ok := ch.(MessageEditor); ok {
						if payloadEditor, ok := ch.(MessageEditorWithPayload); ok {
							_ = payloadEditor.EditMessageWithPayload(
								ctx,
								chatID,
								entry.id,
								outboundMessageEditPayload(msg, msg.Content),
							)
						} else {
							editor.EditMessage(ctx, chatID, entry.id, msg.Content) // 回退
						}
					}
				}
			}
			if !isToolFeedback {
				if separateToolFeedbackMessages {
					clearTrackedToolFeedbackMessage(ch, chatID, &msg.Context)
				} else {
					dismissTrackedToolFeedbackMessage(ctx, ch, chatID, &msg.Context)
				}
			}
			return nil, true
		}
	}

	if _, loaded := m.streamActive.Load(streamKey); loaded {
		return nil, false
	}
	if m.streamActiveForChat(name, chatID) {
		return nil, false
	}

	if !isAuxiliaryMessage {
		m.streamAuxiliaryTombstones.Delete(streamKey)
	}

	if separateToolFeedbackMessages {
		clearTrackedToolFeedbackMessage(ch, chatID, &msg.Context)
	}

	// 5. 尝试编辑占位符
	if v, loaded := m.placeholders.LoadAndDelete(key); loaded {
		if entry, ok := v.(placeholderEntry); ok && entry.id != "" {
			if isToolFeedback && separateToolFeedbackMessages {
				if deleter, ok := ch.(MessageDeleter); ok {
					deleter.DeleteMessage(ctx, chatID, entry.id) // 尽力而为
				}
				return nil, false
			}
			if outboundMessageBypassesPlaceholderEdit(msg) {
				if deleter, ok := ch.(MessageDeleter); ok {
					deleter.DeleteMessage(ctx, chatID, entry.id) // 尽力而为
				}
				return nil, false
			}
			if editor, ok := ch.(MessageEditor); ok {
				content := msg.Content
				trackedContent := msg.Content
				if isToolFeedback {
					trackedContent = prepareToolFeedbackMessageContent(ch, msg.Content)
					content = InitialAnimatedToolFeedbackContent(trackedContent)
				}
				err := func() error {
					if payloadEditor, ok := ch.(MessageEditorWithPayload); ok {
						return payloadEditor.EditMessageWithPayload(
							ctx,
							chatID,
							entry.id,
							outboundMessageEditPayload(msg, content),
						)
					}
					return editor.EditMessage(ctx, chatID, entry.id, content)
				}()
				if err == nil {
					trackedChatID := trackedToolFeedbackMessageChatID(ch, chatID, &msg.Context)
					if tracker, ok := ch.(toolFeedbackMessageTracker); ok && isToolFeedback {
						tracker.RecordToolFeedbackMessage(trackedChatID, entry.id, trackedContent)
					} else if !isToolFeedback {
						dismissTrackedToolFeedbackMessage(ctx, ch, chatID, &msg.Context)
					}
					return []string{entry.id}, true
				}
				// 编辑失败 → 继续走常规 Send
			}
		}
	}

	return nil, false
}

// preSendMedia 在发送媒体附件前处理 typing 停止、reaction 撤销和占位符清理。
// 与文本消息的 preSend 不同，媒体投递从不编辑占位符，因为没有可用来替换的
// 文本载荷；它只会在可能时尝试删除占位符。
func (m *Manager) preSendMedia(ctx context.Context, name string, msg bus.OutboundMediaMessage, ch Channel) {
	chatID := outboundMediaChatID(msg)
	key := name + ":" + chatID
	streamKey := streamSuppressionKey(name, chatID, msg.SessionKey)

	// 1. 停止 typing
	if v, loaded := m.typingStops.LoadAndDelete(key); loaded {
		if entry, ok := v.(typingEntry); ok {
			entry.stop() // 幂等，安全
		}
	}

	// 2. 撤销 reaction
	if v, loaded := m.reactionUndos.LoadAndDelete(key); loaded {
		if entry, ok := v.(reactionEntry); ok {
			entry.undo() // 幂等，安全
		}
	}

	// 3. 投递媒体前先清除该 chat 已终结的 stream 标记。
	m.streamActive.LoadAndDelete(streamKey)
	m.streamAuxiliaryTombstones.Delete(streamKey)

	if m.toolFeedbackSeparateMessagesEnabled() {
		clearTrackedToolFeedbackMessage(ch, chatID, &msg.Context)
	}

	// 4. 若存在占位符则删除。
	if v, loaded := m.placeholders.LoadAndDelete(key); loaded {
		if entry, ok := v.(placeholderEntry); ok && entry.id != "" {
			if deleter, ok := ch.(MessageDeleter); ok {
				deleter.DeleteMessage(ctx, chatID, entry.id) // 尽力而为
			}
		}
	}
}

func NewManager(
	cfg *config.Config,
	messageBus *bus.MessageBus,
	store media.MediaStore,
	opts ...ManagerOption,
) (*Manager, error) {
	m := &Manager{
		channels:      make(map[string]Channel),
		workers:       make(map[string]*channelWorker),
		bus:           messageBus,
		config:        cfg,
		mediaStore:    store,
		channelHashes: make(map[string]string),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}

	// 注册为 streaming delegate，使 agent 循环可以获取 streamer
	messageBus.SetStreamDelegate(m)

	if err := m.initChannels(&cfg.Channels); err != nil {
		return nil, err
	}

	// 保存所有 channel 的初始配置哈希
	m.channelHashes = toChannelHashes(cfg)

	return m, nil
}

// SetMediaStore 更新 manager 以及每个接受媒体存储注入的 channel 所使用的存储。
// Gateway 重载会创建新的存储，因此必须让现有 channel 与 agent 使用同一存储，
// 才能保证重载后入站媒体引用仍可解析。
func (m *Manager) SetMediaStore(store media.MediaStore) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.mediaStore = store
	for _, ch := range m.channels {
		if setter, ok := ch.(mediaStoreSetter); ok {
			setter.SetMediaStore(store)
		}
	}
}

// GetStreamer 实现 bus.StreamDelegate。
// 它检查指定的 channel 是否支持流式输出，并返回一个 Streamer。
func (m *Manager) GetStreamer(ctx context.Context, channelName, chatID, sessionKey string) (bus.Streamer, bool) {
	m.mu.RLock()
	ch, exists := m.channels[channelName]
	m.mu.RUnlock()

	if !exists {
		return nil, false
	}

	sc, ok := ch.(StreamingCapable)
	if !ok {
		return nil, false
	}

	streamer, err := sc.BeginStream(ctx, chatID)
	if err != nil {
		logger.DebugCF("channels", "Streaming unavailable, falling back to placeholder", map[string]any{
			"channel": channelName,
			"error":   err.Error(),
		})
		return nil, false
	}

	// 在 Finalize 时标记 streamActive，使 preSend 知道要清理占位符，
	// 并防止流式输出产生 final 后仍有迟到的辅助消息泄漏。
	streamKey := streamSuppressionKey(channelName, chatID, sessionKey)
	placeholderKey := channelName + ":" + chatID
	clearMarker := func() {
		m.streamActive.Delete(streamKey)
	}
	onFinalize := func(finalizeCtx context.Context, finalContent string) {
		if m.toolFeedbackSeparateMessagesEnabled() {
			clearTrackedToolFeedbackMessage(
				ch,
				chatID,
				&bus.InboundContext{
					Channel: channelName,
					ChatID:  chatID,
				},
			)
		} else {
			dismissTrackedToolFeedbackMessage(
				finalizeCtx,
				ch,
				chatID,
				&bus.InboundContext{
					Channel: channelName,
					ChatID:  chatID,
				},
			)
		}
		if v, loaded := m.placeholders.LoadAndDelete(placeholderKey); loaded {
			if entry, ok := v.(placeholderEntry); ok && entry.id != "" {
				if deleter, ok := ch.(MessageDeleter); ok {
					deleter.DeleteMessage(finalizeCtx, chatID, entry.id) // 尽力而为
				} else if editor, ok := ch.(MessageEditor); ok {
					editor.EditMessage(finalizeCtx, chatID, entry.id, finalContent) // 尽力而为的回退
				}
			}
		}
		m.streamActive.Store(streamKey, true)
		m.streamAuxiliaryTombstones.Store(streamKey, time.Now())
	}

	if m.config != nil && m.config.Agents.Defaults.SplitOnMarker {
		return &splitMarkerStreamer{
			current:     streamer,
			reasoning:   reasoningStreamerFrom(streamer),
			begin:       func(beginCtx context.Context) (bus.Streamer, error) { return sc.BeginStream(beginCtx, chatID) },
			onFinalize:  onFinalize,
			clearMarker: clearMarker,
		}, true
	}

	return &finalizeHookStreamer{
		Streamer:    streamer,
		clearMarker: clearMarker,
		onFinalize:  onFinalize,
	}, true
}

func reasoningStreamerFrom(streamer bus.Streamer) bus.ReasoningStreamer {
	if reasoningStreamer, ok := streamer.(bus.ReasoningStreamer); ok {
		return reasoningStreamer
	}
	return nil
}

type modelNameStreamer interface {
	SetModelName(modelName string)
}

func setStreamerModelName(streamer any, modelName string) {
	setter, ok := streamer.(modelNameStreamer)
	if !ok {
		return
	}
	setter.SetModelName(modelName)
}

// splitMarkerStreamer 将包含 MessageSplitMarker 的累积流式文本
// 拆分为多条独立的 channel 流式消息。
type splitMarkerStreamer struct {
	mu             sync.Mutex
	current        bus.Streamer
	reasoning      bus.ReasoningStreamer
	begin          func(context.Context) (bus.Streamer, error)
	completedParts int
	finalized      bool
	onFinalize     func(context.Context, string)
	clearMarker    func()
	modelName      string
}

func (s *splitMarkerStreamer) Update(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(ctx, content)
}

func (s *splitMarkerStreamer) Finalize(ctx context.Context, content string) error {
	return s.FinalizeWithContext(ctx, content, nil)
}

func (s *splitMarkerStreamer) FinalizeWithContext(ctx context.Context, content string, usage *bus.ContextUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.finalizeLocked(ctx, content, usage); err != nil {
		return err
	}
	s.runFinalizeHook(ctx, content)
	return nil
}

func (s *splitMarkerStreamer) UpdateReasoning(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reasoning == nil {
		return nil
	}
	setStreamerModelName(s.reasoning, s.modelName)
	return s.reasoning.UpdateReasoning(ctx, content)
}

func (s *splitMarkerStreamer) FinalizeReasoning(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reasoning == nil {
		return nil
	}
	setStreamerModelName(s.reasoning, s.modelName)
	return s.reasoning.FinalizeReasoning(ctx, content)
}

func (s *splitMarkerStreamer) SetModelName(modelName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelName = strings.TrimSpace(modelName)
	setStreamerModelName(s.current, s.modelName)
	setStreamerModelName(s.reasoning, s.modelName)
}

func (s *splitMarkerStreamer) Cancel(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		s.current.Cancel(ctx)
	}
}

func (s *splitMarkerStreamer) ClearFinalizedStreamMarker() {
	if s.clearMarker != nil {
		s.clearMarker()
	}
}

func (s *splitMarkerStreamer) updateLocked(ctx context.Context, content string) error {
	parts := strings.Split(content, MessageSplitMarker)
	completedLimit := len(parts) - 1
	if err := s.finalizeCompletedPartsLocked(ctx, parts, completedLimit, nil); err != nil {
		return err
	}
	active := strings.TrimSpace(parts[len(parts)-1])
	if active == "" {
		return nil
	}
	if err := s.ensureCurrentLocked(ctx); err != nil {
		return err
	}
	return s.current.Update(ctx, active)
}

func (s *splitMarkerStreamer) finalizeLocked(ctx context.Context, content string, usage *bus.ContextUsage) error {
	parts := strings.Split(content, MessageSplitMarker)
	return s.finalizeCompletedPartsLocked(ctx, parts, len(parts), usage)
}

func (s *splitMarkerStreamer) finalizeCompletedPartsLocked(
	ctx context.Context,
	parts []string,
	limit int,
	usage *bus.ContextUsage,
) error {
	for s.completedParts < limit {
		content := strings.TrimSpace(parts[s.completedParts])
		isLast := s.completedParts == limit-1
		if content != "" {
			if err := s.ensureCurrentLocked(ctx); err != nil {
				return err
			}
			if isLast && usage != nil {
				if contextStreamer, ok := s.current.(bus.ContextUsageStreamer); ok {
					if err := contextStreamer.FinalizeWithContext(ctx, content, usage); err != nil {
						return err
					}
				} else if err := s.current.Finalize(ctx, content); err != nil {
					return err
				}
			} else if err := s.current.Finalize(ctx, content); err != nil {
				return err
			}
			s.current = nil
		}
		s.completedParts++
	}
	return nil
}

func (s *splitMarkerStreamer) ensureCurrentLocked(ctx context.Context) error {
	if s.current != nil {
		return nil
	}
	if s.begin == nil {
		return fmt.Errorf("streamer is not initialized")
	}
	streamer, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.current = streamer
	setStreamerModelName(s.current, s.modelName)
	return nil
}

func (s *splitMarkerStreamer) runFinalizeHook(ctx context.Context, content string) {
	if s.finalized {
		return
	}
	s.finalized = true
	if s.onFinalize != nil {
		s.onFinalize(ctx, content)
	}
}

func (m *Manager) streamAuxiliaryTombstoneActive(key string) bool {
	v, ok := m.streamAuxiliaryTombstones.Load(key)
	if !ok {
		return false
	}
	createdAt, ok := v.(time.Time)
	if !ok || time.Since(createdAt) > streamAuxiliaryTombstoneTTL {
		m.streamAuxiliaryTombstones.Delete(key)
		return false
	}
	return true
}

func (m *Manager) streamActiveForChat(channel, chatID string) bool {
	chatKey := streamSuppressionKey(channel, chatID, "")
	found := false
	m.streamActive.Range(func(key, _ any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		if keyString == chatKey || strings.HasPrefix(keyString, chatKey+":") {
			found = true
			return false
		}
		return true
	})
	return found
}

// finalizeHookStreamer 包装一个 Streamer，以便在 Finalize 时执行钩子。
type finalizeHookStreamer struct {
	Streamer
	onFinalize  func(context.Context, string)
	clearMarker func()
}

func (s *finalizeHookStreamer) Finalize(ctx context.Context, content string) error {
	if err := s.Streamer.Finalize(ctx, content); err != nil {
		return err
	}
	s.runFinalizeHook(ctx, content)
	return nil
}

func (s *finalizeHookStreamer) FinalizeWithContext(ctx context.Context, content string, usage *bus.ContextUsage) error {
	if streamer, ok := s.Streamer.(bus.ContextUsageStreamer); ok {
		if err := streamer.FinalizeWithContext(ctx, content, usage); err != nil {
			return err
		}
	} else if err := s.Streamer.Finalize(ctx, content); err != nil {
		return err
	}
	s.runFinalizeHook(ctx, content)
	return nil
}

func (s *finalizeHookStreamer) UpdateReasoning(ctx context.Context, content string) error {
	if streamer, ok := s.Streamer.(bus.ReasoningStreamer); ok {
		return streamer.UpdateReasoning(ctx, content)
	}
	return nil
}

func (s *finalizeHookStreamer) FinalizeReasoning(ctx context.Context, content string) error {
	if streamer, ok := s.Streamer.(bus.ReasoningStreamer); ok {
		return streamer.FinalizeReasoning(ctx, content)
	}
	return nil
}

func (s *finalizeHookStreamer) SetModelName(modelName string) {
	setStreamerModelName(s.Streamer, strings.TrimSpace(modelName))
}

func (s *finalizeHookStreamer) runFinalizeHook(ctx context.Context, content string) {
	if s.onFinalize != nil {
		s.onFinalize(ctx, content)
	}
}

func (s *finalizeHookStreamer) ClearFinalizedStreamMarker() {
	if s.clearMarker != nil {
		s.clearMarker()
	}
}

// initChannel 是一个辅助函数，按类型名查找工厂并创建 channel。
// typeName 是用于查找工厂的 channel 类型（例如 "telegram"）。
// channelName 是用作 channel 运行时名称的配置 map 键（例如 "my_telegram"）。
func (m *Manager) initChannel(typeName, channelName string) {
	f, ok := getFactory(typeName)
	if !ok {
		logger.WarnCF("channels", "Factory not registered", map[string]any{
			"channel": channelName,
			"type":    typeName,
		})
		return
	}
	logger.DebugCF("channels", "Attempting to initialize channel", map[string]any{
		"channel": channelName,
		"type":    typeName,
	})
	ch, err := f(channelName, typeName, m.config, m.bus)
	if err != nil {
		logger.ErrorCF("channels", "Failed to initialize channel", map[string]any{
			"channel": channelName,
			"type":    typeName,
			"error":   err.Error(),
		})
	} else {
		// 若 channel 支持，则注入 MediaStore
		if m.mediaStore != nil {
			if setter, ok := ch.(mediaStoreSetter); ok {
				setter.SetMediaStore(m.mediaStore)
			}
		}
		// 若 channel 支持，则注入 PlaceholderRecorder
		if setter, ok := ch.(interface{ SetPlaceholderRecorder(r PlaceholderRecorder) }); ok {
			setter.SetPlaceholderRecorder(m)
		}
		// 注入 owner 引用，使 BaseChannel.HandleMessage 能自动触发 typing/reaction
		if setter, ok := ch.(interface{ SetOwner(ch Channel) }); ok {
			setter.SetOwner(ch)
		}
		m.channels[channelName] = ch
		m.publishChannelEvent(
			runtimeevents.KindChannelLifecycleInitialized,
			channelName,
			runtimeevents.Scope{Channel: channelName},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: typeName},
		)
		logger.InfoCF("channels", "Channel enabled successfully", map[string]any{
			"channel": channelName,
			"type":    typeName,
		})
	}
}

func (m *Manager) getChannelConfigAndEnabled(channelName string) (*config.Channel, bool) {
	bc, ok := m.config.Channels[channelName]
	if !ok || bc == nil {
		return nil, false
	}
	if !bc.Enabled {
		return bc, false
	}

	// 用 Type 确定用于校验的配置结构体。
	// map 键（channelName）是配置键，可能与类型不同。
	channelType := bc.Type
	if channelType == "" {
		channelType = channelName
	}

	// 设置已由 InitChannelList 完成解码，因此只需进行类型断言
	// 并检查相关字段。
	decoded, err := bc.GetDecoded()
	if err != nil {
		return bc, false
	}
	//nolint:revive
	switch settings := decoded.(type) {
	case *config.PicoSettings:
		return bc, settings.Token.String() != ""
	case *config.PicoClientSettings:
		return bc, settings.URL != ""
	}

	return bc, bc.Enabled
}

// initChannels 根据配置初始化所有已启用的 channel。
// 它遍历配置条目，并用 bc.Type 查找合适的工厂。
func (m *Manager) initChannels(channels *config.ChannelsConfig) error {
	logger.InfoC("channels", "Initializing channel manager")

	for name, bc := range *channels {
		if !bc.Enabled {
			continue
		}
		_, ready := m.getChannelConfigAndEnabled(name)
		if !ready {
			continue
		}
		typeName := bc.Type
		if typeName == "" {
			typeName = name
		}
		m.initChannel(typeName, name)
	}

	logger.InfoCF("channels", "Channel initialization completed", map[string]any{
		"enabled_channels": len(m.channels),
	})

	return nil
}

// SetupHTTPServer 使用给定的监听地址创建共享 HTTP 服务器。
// 它会注册来自 health server 的健康检查端点，并发现实现了
// WebhookHandler 和/或 HealthChecker 的 channel 以注册它们的处理器。
func (m *Manager) SetupHTTPServer(addr string, healthServer *health.Server) {
	m.SetupHTTPServerListeners(nil, addr, healthServer)
}

// SetupHTTPServerListeners 在已打开的监听器上创建共享 HTTP 服务器。
// 当 listeners 为空时回退到基于 Addr 的 ListenAndServe 行为。
func (m *Manager) SetupHTTPServerListeners(listeners []net.Listener, addr string, healthServer *health.Server) {
	m.mux = newDynamicServeMux()

	// 注册健康检查端点
	if healthServer != nil {
		healthServer.RegisterOnMux(m.mux)
	}

	// 发现并注册 webhook 处理器和健康检查器
	m.registerHTTPHandlersLocked()

	m.httpServer = &http.Server{
		Addr:         addr,
		Handler:      m.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	m.httpListeners = append([]net.Listener(nil), listeners...)
}

// registerHTTPHandlersLocked 为 m.channels 中当前所有 channel
// 注册 webhook 和健康检查处理器。调用方必须持有 m.mu
// （或确保独占访问）。
func (m *Manager) registerHTTPHandlersLocked() {
	for name, ch := range m.channels {
		m.registerChannelHTTPHandler(name, ch)
	}
}

// registerChannelHTTPHandler 将单个 channel 的 webhook/健康检查处理器
// 注册到 m.mux 上。
func (m *Manager) registerChannelHTTPHandler(name string, ch Channel) {
	if wh, ok := ch.(WebhookHandler); ok {
		m.mux.Handle(wh.WebhookPath(), wh)
		m.publishChannelEvent(
			runtimeevents.KindChannelWebhookRegistered,
			name,
			runtimeevents.Scope{Channel: name},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: channelTypeForEvent(m, name)},
		)
		logger.InfoCF("channels", "Webhook handler registered", map[string]any{
			"channel": name,
			"path":    wh.WebhookPath(),
		})
	}
	if hc, ok := ch.(HealthChecker); ok {
		m.mux.HandleFunc(hc.HealthPath(), hc.HealthHandler)
		logger.InfoCF("channels", "Health endpoint registered", map[string]any{
			"channel": name,
			"path":    hc.HealthPath(),
		})
	}
}

// unregisterChannelHTTPHandler 从 m.mux 中移除单个 channel 的
// webhook/健康检查处理器。
func (m *Manager) unregisterChannelHTTPHandler(name string, ch Channel) {
	if wh, ok := ch.(WebhookHandler); ok {
		m.mux.Unhandle(wh.WebhookPath())
		m.publishChannelEvent(
			runtimeevents.KindChannelWebhookUnregistered,
			name,
			runtimeevents.Scope{Channel: name},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: channelTypeForEvent(m, name)},
		)
		logger.InfoCF("channels", "Webhook handler unregistered", map[string]any{
			"channel": name,
			"path":    wh.WebhookPath(),
		})
	}
	if hc, ok := ch.(HealthChecker); ok {
		m.mux.Unhandle(hc.HealthPath())
		logger.InfoCF("channels", "Health endpoint unregistered", map[string]any{
			"channel": name,
			"path":    hc.HealthPath(),
		})
	}
}

func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.channels) == 0 {
		logger.WarnC("channels", "No channels enabled")
	}

	logger.InfoC("channels", "Starting all channels")

	dispatchCtx, cancel := context.WithCancel(ctx)
	m.dispatchCtx = dispatchCtx
	m.dispatchTask = &asyncTask{cancel: cancel}
	failedStarts := make([]error, 0, len(m.channels))
	failedNames := make([]string, 0, len(m.channels))

	for name, channel := range m.channels {
		logger.InfoCF("channels", "Starting channel", map[string]any{
			"channel": name,
		})
		if err := channel.Start(ctx); err != nil {
			logger.ErrorCF("channels", "Failed to start channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
			m.publishChannelEvent(
				runtimeevents.KindChannelLifecycleStartFailed,
				name,
				runtimeevents.Scope{Channel: name},
				runtimeevents.SeverityError,
				ChannelLifecyclePayload{Type: channelTypeForEvent(m, name), Error: err.Error()},
			)
			failedStarts = append(failedStarts, fmt.Errorf("channel %s: %w", name, err))
			failedNames = append(failedNames, name)
			continue
		}
		// 仅在 channel 成功启动后才惰性创建 worker
		channelType := name
		if m.config != nil {
			if bc := m.config.Channels.Get(name); bc != nil && bc.Type != "" {
				channelType = bc.Type
			}
		}
		w := newChannelWorker(name, channel, channelType)
		m.workers[name] = w
		go m.runWorker(dispatchCtx, name, w)
		go m.runMediaWorker(dispatchCtx, name, w)
		m.publishChannelEvent(
			runtimeevents.KindChannelLifecycleStarted,
			name,
			runtimeevents.Scope{Channel: name},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: channelType},
		)
	}

	if len(m.channels) > 0 && len(m.workers) == 0 {
		if m.dispatchTask != nil {
			m.dispatchTask.cancel()
			m.dispatchTask = nil
		}
		m.dispatchCtx = nil

		sort.Strings(failedNames)
		if len(failedStarts) == 0 {
			return fmt.Errorf("failed to start any enabled channels")
		}

		logger.ErrorCF("channels", "All enabled channels failed to start", map[string]any{
			"failed":          len(failedNames),
			"total":           len(m.channels),
			"failed_channels": failedNames,
		})

		return fmt.Errorf("failed to start any enabled channels: %w", errors.Join(failedStarts...))
	}

	if len(failedNames) > 0 {
		sort.Strings(failedNames)
		logger.WarnCF("channels", "Some channels failed to start", map[string]any{
			"failed":          len(failedNames),
			"started":         len(m.workers),
			"total":           len(m.channels),
			"failed_channels": failedNames,
		})
	}

	// 启动从 bus 读取并路由到 worker 的 dispatcher。
	// wait group 让 StopAll 在关闭 worker 队列前 Join 这些 goroutine
	// （否则对 w.queue 的 close 与 send 会构成数据竞争）。
	m.dispatchWG.Add(3)
	go func() {
		defer m.dispatchWG.Done()
		m.dispatchOutbound(dispatchCtx)
	}()
	go func() {
		defer m.dispatchWG.Done()
		m.dispatchOutboundMedia(dispatchCtx)
	}()

	// 启动 TTL janitor，清理过期的 typing/占位符条目
	go func() {
		defer m.dispatchWG.Done()
		m.runTTLJanitor(dispatchCtx)
	}()

	// 若已配置，启动共享 HTTP 服务器
	if m.httpServer != nil {
		if len(m.httpListeners) > 0 {
			for _, listener := range m.httpListeners {
				ln := listener
				go func() {
					defer func() {
						if r := recover(); r != nil {
							logger.ErrorCF("channels", "HTTP server goroutine panic recovered",
								map[string]any{
									"addr":  ln.Addr().String(),
									"panic": fmt.Sprintf("%v", r),
									"stack": string(debug.Stack()),
								})
						}
					}()
					logger.InfoCF("channels", "Shared HTTP server listening", map[string]any{
						"addr": ln.Addr().String(),
					})
					if err := m.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
						logger.FatalCF("channels", "Shared HTTP server error", map[string]any{
							"addr":  ln.Addr().String(),
							"error": err.Error(),
						})
					}
				}()
			}
		} else {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logger.ErrorCF("channels", "HTTP server goroutine panic recovered",
							map[string]any{
								"addr":  m.httpServer.Addr,
								"panic": fmt.Sprintf("%v", r),
								"stack": string(debug.Stack()),
							})
					}
				}()
				logger.InfoCF("channels", "Shared HTTP server listening", map[string]any{
					"addr": m.httpServer.Addr,
				})
				if err := m.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					logger.FatalCF("channels", "Shared HTTP server error", map[string]any{
						"error": err.Error(),
					})
				}
			}()
		}
	}

	logger.InfoCF("channels", "Channel startup completed", map[string]any{
		"started": len(m.workers),
		"failed":  len(failedNames),
		"total":   len(m.channels),
	})
	return nil
}

func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger.InfoC("channels", "Stopping all channels")

	// 先关闭共享 HTTP 服务器
	if m.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := m.httpServer.Shutdown(shutdownCtx); err != nil {
			logger.ErrorCF("channels", "Shared HTTP server shutdown error", map[string]any{
				"error": err.Error(),
			})
		}
		m.httpServer = nil
		m.httpListeners = nil
	}

	// Cancel dispatcher：dispatchCtx 必须是 StartAll 创建、dispatcher 实际使用的那个。
	if m.dispatchTask != nil {
		m.dispatchTask.cancel()
		m.dispatchTask = nil
	}
	m.dispatchCtx = nil

	// 关闭 worker 队列前先 Join dispatcher goroutine：dispatcher 可能仍阻塞在
	// `w.queue <- msg`，对该队列的 close() 绝不能与 send 竞争。
	m.dispatchWG.Wait()

	// 关闭所有 worker 队列并等待其排空
	for _, w := range m.workers {
		if w != nil {
			close(w.queue)
		}
	}
	for _, w := range m.workers {
		if w != nil {
			<-w.done
		}
	}
	// 关闭所有媒体 worker 队列并等待其排空
	for _, w := range m.workers {
		if w != nil {
			close(w.mediaQueue)
		}
	}
	for _, w := range m.workers {
		if w != nil {
			<-w.mediaDone
		}
	}

	// 停止所有 channel
	for name, channel := range m.channels {
		logger.InfoCF("channels", "Stopping channel", map[string]any{
			"channel": name,
		})
		if err := channel.Stop(ctx); err != nil {
			logger.ErrorCF("channels", "Error stopping channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
			continue
		}
		m.publishChannelEvent(
			runtimeevents.KindChannelLifecycleStopped,
			name,
			runtimeevents.Scope{Channel: name},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: channelTypeForEvent(m, name)},
		)
	}

	logger.InfoC("channels", "All channels stopped")
	return nil
}

// newChannelWorker 创建一个带速率限制器的 channelWorker，
// 速率限制器针对给定的 channel 类型进行配置。channelType 用于查询速率限制。
func newChannelWorker(name string, ch Channel, channelType string) *channelWorker {
	rateVal := float64(defaultRateLimit)
	if r, ok := channelRateConfig[channelType]; ok {
		rateVal = r
	}
	burst := int(math.Max(1, math.Ceil(rateVal/2)))

	return &channelWorker{
		ch:         ch,
		queue:      make(chan bus.OutboundMessage, defaultChannelQueueSize),
		mediaQueue: make(chan bus.OutboundMediaMessage, defaultChannelQueueSize),
		done:       make(chan struct{}),
		mediaDone:  make(chan struct{}),
		limiter:    rate.NewLimiter(rate.Limit(rateVal), burst),
	}
}

// runWorker 处理单个 channel 的出站消息。
// 消息处理按以下顺序进行：
//  1. SplitByMarker（若配置中启用）- 基于 LLM 语义标记的拆分
//  2. SplitMessage - 按 channel 特定长度的拆分（MaxMessageLength）
func (m *Manager) runWorker(ctx context.Context, name string, w *channelWorker) {
	defer close(w.done)
	for {
		select {
		case msg, ok := <-w.queue:
			if !ok {
				return
			}
			maxLen := 0
			if mlp, ok := w.ch.(MessageLengthProvider); ok {
				maxLen = mlp.MaxMessageLength()
			}

			// 收集所有待发送的消息分块
			var chunks []string

			// 步骤 1：若已启用，尝试基于标记的拆分。
			// tool feedback 必须保持为单条消息，因此跳过标记拆分。
			// stream-final 的重复响应也必须保持完整，以便 preSend 能在任何
			// 标记分块泄漏之前消费整条 final 消息。
			if m.finalizedStreamActiveForMessage(name, msg) {
				chunks = []string{msg.Content}
			} else if m.config != nil && m.config.Agents.Defaults.SplitOnMarker && !outboundMessageIsToolFeedback(msg) {
				if markerChunks := SplitByMarker(msg.Content); len(markerChunks) > 1 {
					for _, chunk := range markerChunks {
						chunkMsg := msg
						chunkMsg.Content = chunk
						chunks = append(chunks, splitOutboundMessageContent(chunkMsg, maxLen)...)
					}
				}
			}

			// 步骤 2：若标记拆分没有产生分块，则回退到按长度拆分
			if len(chunks) == 0 {
				chunks = splitOutboundMessageContent(msg, maxLen)
			}

			// 步骤 3：发送所有分块
			for _, chunk := range chunks {
				chunkMsg := msg
				chunkMsg.Content = chunk
				m.sendWithRetry(ctx, name, w, chunkMsg)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) finalizedStreamActiveForMessage(channelName string, msg bus.OutboundMessage) bool {
	if m == nil || !outboundMessageIsFinal(msg) {
		return false
	}
	chatID := outboundMessageChatID(msg)
	if strings.TrimSpace(channelName) == "" || strings.TrimSpace(chatID) == "" {
		return false
	}
	_, active := m.streamActive.Load(streamSuppressionKey(channelName, chatID, msg.SessionKey))
	return active
}

// splitOutboundMessageContent 按 maxLen 拆分常规出站内容，
// 但会通过截断说明正文来让 tool feedback 保持为单条消息。
func splitOutboundMessageContent(msg bus.OutboundMessage, maxLen int) []string {
	if maxLen > 0 {
		if outboundMessageIsToolFeedback(msg) {
			animationSafeLen := maxLen - MaxToolFeedbackAnimationFrameLength()
			if animationSafeLen <= 0 {
				animationSafeLen = maxLen
			}
			if len([]rune(msg.Content)) > animationSafeLen {
				return []string{utils.FitToolFeedbackMessage(msg.Content, animationSafeLen)}
			}
			return []string{msg.Content}
		}
		if len([]rune(msg.Content)) > maxLen {
			return SplitMessage(msg.Content, maxLen)
		}
	}
	return []string{msg.Content}
}

// sendWithRetry 通过 channel 发送消息，并带有速率限制和重试逻辑。
// 它会按错误类型决定重试策略：
//   - ErrNotRunning / ErrSendFailed：永久性失败，不重试
//   - ErrRateLimit：固定延迟重试
//   - ErrTemporary / 未知错误：指数退避重试
func (m *Manager) sendWithRetry(
	ctx context.Context,
	name string,
	w *channelWorker,
	msg bus.OutboundMessage,
) ([]string, bool) {
	// 速率限制：等待令牌
	if err := w.limiter.Wait(ctx); err != nil {
		// ctx 被取消，正在关闭
		m.publishChannelEvent(
			runtimeevents.KindChannelRateLimited,
			name,
			scopeFromOutboundContext(msg.Context),
			runtimeevents.SeverityWarn,
			ChannelOutboundPayload{
				ContentLen:       len([]rune(msg.Content)),
				ReplyToMessageID: msg.ReplyToMessageID,
				Error:            err.Error(),
			},
		)
		return nil, false
	}

	// 发送前：停止 typing 并尝试编辑占位符
	if msgIDs, handled := m.preSend(ctx, name, msg, w.ch); handled {
		m.publishOutboundSent(name, msg, msgIDs)
		return msgIDs, true
	}

	var lastErr error
	var msgIDs []string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msgIDs, lastErr = w.ch.Send(ctx, msg)
		if lastErr == nil {
			m.publishOutboundSent(name, msg, msgIDs)
			return msgIDs, true
		}

		// 永久性失败 — 不重试
		if errors.Is(lastErr, ErrNotRunning) || errors.Is(lastErr, ErrSendFailed) {
			break
		}

		// 最后一次尝试已用尽 — 不再休眠
		if attempt == maxRetries {
			break
		}

		// 限流错误 — 固定延迟
		if errors.Is(lastErr, ErrRateLimit) {
			select {
			case <-time.After(rateLimitDelay):
				continue
			case <-ctx.Done():
				return nil, false
			}
		}

		// ErrTemporary 或未知错误 — 指数退避
		backoff := min(time.Duration(float64(baseBackoff)*math.Pow(2, float64(attempt))), maxBackoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, false
		}
	}

	// 所有重试已用尽或永久性失败
	logger.ErrorCF("channels", "Send failed", map[string]any{
		"channel": name,
		"chat_id": outboundMessageChatID(msg),
		"error":   lastErr.Error(),
		"retries": maxRetries,
	})
	m.publishOutboundFailed(name, msg, lastErr, false)

	return nil, false
}

func dispatchLoop[M any](
	ctx context.Context,
	m *Manager,
	ch <-chan M,
	getChannel func(M) string,
	enqueue func(context.Context, *channelWorker, M) bool,
	startMsg, stopMsg, unknownMsg, noWorkerMsg string,
) {
	logger.InfoC("channels", startMsg)

	for {
		select {
		case <-ctx.Done():
			logger.InfoC("channels", stopMsg)
			return

		case msg, ok := <-ch:
			if !ok {
				logger.InfoC("channels", stopMsg)
				return
			}

			channel := getChannel(msg)

			// 静默跳过内部 channel
			if constants.IsInternalChannel(channel) {
				continue
			}

			m.mu.RLock()
			_, exists := m.channels[channel]
			w, wExists := m.workers[channel]
			m.mu.RUnlock()

			if !exists {
				logger.WarnCF("channels", unknownMsg, map[string]any{"channel": channel})
				continue
			}

			if wExists && w != nil {
				if !enqueue(ctx, w, msg) {
					return
				}
			} else if exists {
				logger.WarnCF("channels", noWorkerMsg, map[string]any{"channel": channel})
			}
		}
	}
}

func (m *Manager) dispatchOutbound(ctx context.Context) {
	dispatchLoop(
		ctx, m,
		m.bus.OutboundChan(),
		func(msg bus.OutboundMessage) string { return outboundMessageChannel(msg) },
		func(ctx context.Context, w *channelWorker, msg bus.OutboundMessage) bool {
			select {
			case w.queue <- msg:
				m.publishOutboundQueued(outboundMessageChannel(msg), msg)
				return true
			case <-ctx.Done():
				return false
			}
		},
		"Outbound dispatcher started",
		"Outbound dispatcher stopped",
		"Unknown channel for outbound message",
		"Channel has no active worker, skipping message",
	)
}

func (m *Manager) dispatchOutboundMedia(ctx context.Context) {
	dispatchLoop(
		ctx, m,
		m.bus.OutboundMediaChan(),
		func(msg bus.OutboundMediaMessage) string { return outboundMediaChannel(msg) },
		func(ctx context.Context, w *channelWorker, msg bus.OutboundMediaMessage) bool {
			select {
			case w.mediaQueue <- msg:
				m.publishOutboundMediaQueued(outboundMediaChannel(msg), msg)
				return true
			case <-ctx.Done():
				return false
			}
		},
		"Outbound media dispatcher started",
		"Outbound media dispatcher stopped",
		"Unknown channel for outbound media message",
		"Channel has no active worker, skipping media message",
	)
}

// runMediaWorker 处理单个 channel 的出站媒体消息。
func (m *Manager) runMediaWorker(ctx context.Context, name string, w *channelWorker) {
	defer close(w.mediaDone)
	for {
		select {
		case msg, ok := <-w.mediaQueue:
			if !ok {
				return
			}
			_, _ = m.sendMediaWithRetry(ctx, name, w, msg)
		case <-ctx.Done():
			return
		}
	}
}

// sendMediaWithRetry 通过 channel 发送媒体消息，并带有速率限制和重试逻辑。
// 成功时返回消息 ID 和 nil；重试耗尽后返回 nil 和最后一个错误，
// 也包括 channel 不支持 MediaSender 的情况。
func (m *Manager) sendMediaWithRetry(
	ctx context.Context,
	name string,
	w *channelWorker,
	msg bus.OutboundMediaMessage,
) ([]string, error) {
	ms, ok := w.ch.(MediaSender)
	if !ok {
		err := fmt.Errorf("channel %q does not support media sending", name)
		logger.WarnCF("channels", "Channel does not support MediaSender", map[string]any{
			"channel": name,
			"error":   err.Error(),
		})
		return nil, err
	}

	// 速率限制：等待令牌
	if err := w.limiter.Wait(ctx); err != nil {
		m.publishChannelEvent(
			runtimeevents.KindChannelRateLimited,
			name,
			scopeFromOutboundContext(msg.Context),
			runtimeevents.SeverityWarn,
			ChannelOutboundPayload{
				Media: true,
				Error: err.Error(),
			},
		)
		return nil, err
	}

	// 发送前：停止 typing，并在发送媒体前清理所有占位符。
	m.preSendMedia(ctx, name, msg, w.ch)

	var lastErr error
	var msgIDs []string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msgIDs, lastErr = ms.SendMedia(ctx, msg)
		if lastErr == nil {
			m.publishOutboundMediaSent(name, msg, msgIDs)
			return msgIDs, nil
		}

		// 永久性失败 — 不重试
		if errors.Is(lastErr, ErrNotRunning) || errors.Is(lastErr, ErrSendFailed) {
			break
		}

		// 最后一次尝试已用尽 — 不再休眠
		if attempt == maxRetries {
			break
		}

		// 限流错误 — 固定延迟
		if errors.Is(lastErr, ErrRateLimit) {
			select {
			case <-time.After(rateLimitDelay):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// ErrTemporary 或未知错误 — 指数退避
		backoff := min(time.Duration(float64(baseBackoff)*math.Pow(2, float64(attempt))), maxBackoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 所有重试已用尽或永久性失败
	logger.ErrorCF("channels", "SendMedia failed", map[string]any{
		"channel": name,
		"chat_id": outboundMediaChatID(msg),
		"error":   lastErr.Error(),
		"retries": maxRetries,
	})
	m.publishOutboundMediaFailed(name, msg, lastErr)
	return nil, lastErr
}

// runTTLJanitor 定期扫描 typingStops、placeholders 和 stream
// tombstone map，淘汰已超过 TTL 的条目。这可以防止出站路径
// 未能触发 preSend（例如 LLM 出错）时内存不断累积。
func (m *Manager) runTTLJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.typingStops.Range(func(key, value any) bool {
				if entry, ok := value.(typingEntry); ok {
					if now.Sub(entry.createdAt) > typingStopTTL {
						if _, loaded := m.typingStops.LoadAndDelete(key); loaded {
							entry.stop() // 幂等，安全
						}
					}
				}
				return true
			})
			m.reactionUndos.Range(func(key, value any) bool {
				if entry, ok := value.(reactionEntry); ok {
					if now.Sub(entry.createdAt) > typingStopTTL {
						if _, loaded := m.reactionUndos.LoadAndDelete(key); loaded {
							entry.undo() // 幂等，安全
						}
					}
				}
				return true
			})
			m.placeholders.Range(func(key, value any) bool {
				if entry, ok := value.(placeholderEntry); ok {
					if now.Sub(entry.createdAt) > placeholderTTL {
						m.placeholders.Delete(key)
					}
				}
				return true
			})
			m.streamAuxiliaryTombstones.Range(func(key, value any) bool {
				if createdAt, ok := value.(time.Time); !ok || now.Sub(createdAt) > streamAuxiliaryTombstoneTTL {
					m.streamAuxiliaryTombstones.Delete(key)
				}
				return true
			})
		}
	}
}

func (m *Manager) GetChannel(name string) (Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	channel, ok := m.channels[name]
	return channel, ok
}

func (m *Manager) GetStatus() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make(map[string]any)
	for name, channel := range m.channels {
		status[name] = map[string]any{
			"enabled": true,
			"running": channel.IsRunning(),
		}
	}
	return status
}

func (m *Manager) GetEnabledChannels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.channels))
	for name := range m.channels {
		names = append(names, name)
	}
	return names
}

// Reload 更新配置引用而不重启 channel。
// 用于 channel 配置未变、但配置的其他部分发生变化的情况。
func (m *Manager) Reload(ctx context.Context, cfg *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 保存旧配置，以便出错时回滚。
	oldConfig := m.config

	// 提前更新配置：initChannel 会通过 factory(m.config, m.bus) 使用 m.config。
	m.config = cfg

	list := toChannelHashes(cfg)
	added, removed := compareChannels(m.channelHashes, list)

	deferFuncs := make([]func(), 0, len(removed)+len(added))
	for _, name := range removed {
		// 停止所有 channel
		channel := m.channels[name]
		logger.InfoCF("channels", "Stopping channel", map[string]any{
			"channel": name,
		})
		if err := channel.Stop(ctx); err != nil {
			logger.ErrorCF("channels", "Error stopping channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
		}
		deferFuncs = append(deferFuncs, func() {
			m.UnregisterChannel(name)
		})
	}
	// dispatcher 由 StartAll 启动、由 StopAll 关闭。Reload 必须复用同一个
	// dispatchCtx：若在这里新建 cancel 覆盖 dispatchTask，旧 dispatcher 仍使用
	// 启动时 ctx，SIGTERM 时 StopAll 的 cancel 对它们是孤儿操作，
	// dispatchWG.Wait() 会永久阻塞（reload→SIGTERM 死锁）。
	dispatchCtx := m.dispatchCtx
	if dispatchCtx == nil {
		// 防御路径：未经过 StartAll 的 Reload（测试/异常）仍要有可取消上下文。
		var cancel context.CancelFunc
		dispatchCtx, cancel = context.WithCancel(ctx)
		m.dispatchCtx = dispatchCtx
		m.dispatchTask = &asyncTask{cancel: cancel}
	}
	cc, err := toChannelConfig(cfg, added)
	if err != nil {
		logger.ErrorC("channels", fmt.Sprintf("toChannelConfig error: %v", err))
		m.config = oldConfig
		return err
	}
	err = m.initChannels(cc)
	if err != nil {
		logger.ErrorC("channels", fmt.Sprintf("initChannels error: %v", err))
		m.config = oldConfig
		return err
	}
	for _, name := range added {
		channel := m.channels[name]
		logger.InfoCF("channels", "Starting channel", map[string]any{
			"channel": name,
		})
		if err := channel.Start(ctx); err != nil {
			logger.ErrorCF("channels", "Failed to start channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
			m.publishChannelEvent(
				runtimeevents.KindChannelLifecycleStartFailed,
				name,
				runtimeevents.Scope{Channel: name},
				runtimeevents.SeverityError,
				ChannelLifecyclePayload{Type: channelTypeForEvent(m, name), Error: err.Error()},
			)
			continue
		}
		// 仅在 channel 成功启动后才惰性创建 worker
		channelType := name
		if m.config != nil {
			if bc := m.config.Channels.Get(name); bc != nil && bc.Type != "" {
				channelType = bc.Type
			}
		}
		w := newChannelWorker(name, channel, channelType)
		m.workers[name] = w
		go m.runWorker(dispatchCtx, name, w)
		go m.runMediaWorker(dispatchCtx, name, w)
		m.publishChannelEvent(
			runtimeevents.KindChannelLifecycleStarted,
			name,
			runtimeevents.Scope{Channel: name},
			runtimeevents.SeverityInfo,
			ChannelLifecyclePayload{Type: channelType},
		)
		deferFuncs = append(deferFuncs, func() {
			m.RegisterChannel(name, channel)
		})
	}

	// 仅在完全成功后才提交哈希。
	m.channelHashes = list
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("channels", "channel registration goroutine panic recovered",
					map[string]any{
						"panic": fmt.Sprintf("%v", r),
						"stack": string(debug.Stack()),
					})
			}
		}()
		for _, f := range deferFuncs {
			f()
		}
	}()
	return nil
}

func (m *Manager) RegisterChannel(name string, channel Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[name] = channel
	if m.mux != nil {
		m.registerChannelHTTPHandler(name, channel)
	}
}

func (m *Manager) UnregisterChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.channels[name]; ok && m.mux != nil {
		m.unregisterChannelHTTPHandler(name, ch)
	}
	if w, ok := m.workers[name]; ok && w != nil {
		close(w.queue)
		<-w.done
		close(w.mediaQueue)
		<-w.mediaDone
	}
	delete(m.workers, name)
	delete(m.channels, name)
}

// SendMessage 通过 channel worker 的速率限制器和重试逻辑同步发送一条出站消息。
// 它会阻塞直到消息投递完成（或所有重试耗尽），从而在后续操作
// 依赖该消息已发送时仍能保持顺序。
func (m *Manager) SendMessage(ctx context.Context, msg bus.OutboundMessage) error {
	msg = bus.NormalizeOutboundMessage(msg)
	channelName := outboundMessageChannel(msg)

	m.mu.RLock()
	_, exists := m.channels[channelName]
	w, wExists := m.workers[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}
	if !wExists || w == nil {
		return fmt.Errorf("channel %s has no active worker", channelName)
	}

	maxLen := 0
	if mlp, ok := w.ch.(MessageLengthProvider); ok {
		maxLen = mlp.MaxMessageLength()
	}
	if chunks := splitOutboundMessageContent(msg, maxLen); len(chunks) > 1 {
		for _, chunk := range chunks {
			chunkMsg := msg
			chunkMsg.Content = chunk
			m.sendWithRetry(ctx, channelName, w, chunkMsg)
		}
	} else {
		if len(chunks) == 1 {
			msg.Content = chunks[0]
		}
		m.sendWithRetry(ctx, channelName, w, msg)
	}
	return nil
}

// SendMedia 通过 channel worker 的速率限制器和重试逻辑同步发送出站媒体。
// 它会阻塞直到媒体投递完成（或所有重试耗尽），从而在后续 agent 行为
// 依赖媒体实际投递时仍能保持顺序。
func (m *Manager) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	msg = bus.NormalizeOutboundMediaMessage(msg)
	channelName := outboundMediaChannel(msg)

	m.mu.RLock()
	_, exists := m.channels[channelName]
	w, wExists := m.workers[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}
	if !wExists || w == nil {
		return fmt.Errorf("channel %s has no active worker", channelName)
	}

	_, err := m.sendMediaWithRetry(ctx, channelName, w, msg)
	return err
}

func (m *Manager) SendToChannel(ctx context.Context, channelName, chatID, content string) error {
	m.mu.RLock()
	_, exists := m.channels[channelName]
	w, wExists := m.workers[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}

	msg := bus.OutboundMessage{
		Context: bus.NewOutboundContext(channelName, chatID, ""),
		Content: content,
	}
	msg = bus.NormalizeOutboundMessage(msg)

	if wExists && w != nil {
		select {
		case w.queue <- msg:
			m.publishOutboundQueued(channelName, msg)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// 回退：直接发送（不应发生）
	channel, _ := m.channels[channelName]
	_, err := channel.Send(ctx, msg)
	return err
}
