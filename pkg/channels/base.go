package channels

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/identity"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
)

var (
	uniqueIDCounter uint64
	uniqueIDPrefix  string
)

func init() {
	// 从 crypto/rand 一次性读取随机字节以生成唯一前缀（仅一次系统调用）。
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 回退到基于时间的前缀
		binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	}
	uniqueIDPrefix = hex.EncodeToString(b[:])
}

// uniqueID 使用随机前缀与原子计数器生成进程内唯一的 ID。
// 该 ID 用于内部关联（例如 media scope key），并非密码学安全——
// 不得用于需要不可预测性的场景。
func uniqueID() string {
	n := atomic.AddUint64(&uniqueIDCounter, 1)
	return uniqueIDPrefix + strconv.FormatUint(n, 16)
}

type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error)
	IsRunning() bool
	IsAllowed(senderID string) bool
	IsAllowedSender(sender bus.SenderInfo) bool
	ReasoningChannelID() string
}

// BaseChannelOption 是用于配置 BaseChannel 的函数式选项。
type BaseChannelOption func(*BaseChannel)

// WithMaxMessageLength 设置渠道的最大消息长度（以 rune 计）。
// 超过该限制的消息会由 Manager 自动拆分。
// 值为 0 表示不限制。
func WithMaxMessageLength(n int) BaseChannelOption {
	return func(c *BaseChannel) { c.maxMessageLength = n }
}

// WithGroupTrigger 设置渠道的群组触发配置。
func WithGroupTrigger(gt config.GroupTriggerConfig) BaseChannelOption {
	return func(c *BaseChannel) { c.groupTrigger = gt }
}

// WithReasoningChannelID 设置应发送思考内容的推理渠道 ID。
func WithReasoningChannelID(id string) BaseChannelOption {
	return func(c *BaseChannel) { c.reasoningChannelID = id }
}

// MessageLengthProvider 是渠道可选实现的接口，用于声明其最大消息长度。
// Manager 通过类型断言使用它来决定是否拆分出站消息。
type MessageLengthProvider interface {
	MaxMessageLength() int
}

type BaseChannel struct {
	config              any
	bus                 *bus.MessageBus
	running             atomic.Bool
	name                string
	allowList           []string
	maxMessageLength    int
	groupTrigger        config.GroupTriggerConfig
	mediaStore          media.MediaStore
	placeholderRecorder PlaceholderRecorder
	owner               Channel // 内嵌该 BaseChannel 的具体渠道
	reasoningChannelID  string
}

func NewBaseChannel(
	name string,
	config any,
	bus *bus.MessageBus,
	allowList []string,
	opts ...BaseChannelOption,
) *BaseChannel {
	isEmpty := true
	for _, s := range allowList {
		if s != "" {
			isEmpty = false
			break
		}
	}
	if isEmpty {
		allowList = []string{}
	}
	bc := &BaseChannel{
		config:    config,
		bus:       bus,
		name:      name,
		allowList: allowList,
	}
	for _, opt := range opts {
		opt(bc)
	}

	// 安全审计：检查是否使用了默认开放（不安全）的渠道。
	// Chagent 以默认安全为目标。若 allow_from 为空，机器人
	// 当前会默认接受来自任何人的消息。若要有意确认并允许
	// 这种情况（例如用于公开机器人），请使用 ["*"]。
	if len(bc.allowList) == 0 {
		logger.WarnCF("channels", "SECURITY: Channel allows EVERYONE (allow_from is empty)", map[string]any{
			"channel": bc.name,
			"hint":    "Set allow_from to your ID, or use '*' to explicitly acknowledge open access.",
		})
	}

	return bc
}

// MaxMessageLength 返回该渠道的最大消息长度（以 rune 计）。
// 值为 0 表示不限制。
func (c *BaseChannel) MaxMessageLength() int {
	return c.maxMessageLength
}

// ShouldRespondInGroup 判断机器人在群聊中是否应当回复。
// 各渠道负责：
//  1. 检测 isMentioned（平台相关）
//  2. 从内容中剥离对机器人的提及（平台相关）
//  3. 调用本方法获取群聊响应决策
//
// 逻辑：
//   - 若 isMentioned → 总是响应
//   - 若配置了 mention_only 且未被提及 → 忽略
//   - 若配置了 prefixes → 内容以任一前缀开头则响应（并剥离该前缀）
//   - 若配置了 prefixes 但均未匹配且未被提及 → 忽略
//   - 其他情况（未配置 group_trigger）→ 响应所有消息（宽松默认）
func (c *BaseChannel) ShouldRespondInGroup(isMentioned bool, content string) (bool, string) {
	gt := c.groupTrigger

	// 被提及 → 总是响应
	if isMentioned {
		return true, strings.TrimSpace(content)
	}

	// mention_only → 必须被提及
	if gt.MentionOnly {
		return false, content
	}

	// 前缀匹配
	if len(gt.Prefixes) > 0 {
		for _, prefix := range gt.Prefixes {
			if prefix != "" && strings.HasPrefix(content, prefix) {
				return true, strings.TrimSpace(strings.TrimPrefix(content, prefix))
			}
		}
		// 配置了前缀但均未匹配，且未被提及 → 忽略
		return false, content
	}

	// 未配置 group_trigger → 宽松处理（响应所有消息）
	return true, strings.TrimSpace(content)
}

func (c *BaseChannel) Name() string {
	return c.name
}

// SetName 更新渠道名称。由 manager 在创建渠道后调用，
// 以确保名称与配置键一致（配置键可能与类型不同）。
func (c *BaseChannel) SetName(name string) {
	c.name = name
}

func (c *BaseChannel) ReasoningChannelID() string {
	return c.reasoningChannelID
}

func (c *BaseChannel) IsRunning() bool {
	return c.running.Load()
}

func (c *BaseChannel) IsAllowed(senderID string) bool {
	if len(c.allowList) == 0 {
		return true
	}

	// 从形如 "123456|username" 的复合 senderID 中提取各部分
	idPart := senderID
	userPart := ""
	if idx := strings.Index(senderID, "|"); idx > 0 {
		idPart = senderID[:idx]
		userPart = senderID[idx+1:]
	}

	for _, allowed := range c.allowList {
		if allowed == "*" {
			return true
		}
		// 进行用户名匹配时去掉 allowed 值开头的 "@"
		trimmed := strings.TrimPrefix(allowed, "@")
		allowedID := trimmed
		allowedUser := ""
		if idx := strings.Index(trimmed, "|"); idx > 0 {
			allowedID = trimmed[:idx]
			allowedUser = trimmed[idx+1:]
		}

		// 支持任一侧使用 "id|username" 复合形式。
		// 这样可保持与旧版 Telegram 允许列表条目的向后兼容。
		if senderID == allowed ||
			idPart == allowed ||
			senderID == trimmed ||
			idPart == trimmed ||
			idPart == allowedID ||
			(allowedUser != "" && senderID == allowedUser) ||
			(userPart != "" && (userPart == allowed || userPart == trimmed || userPart == allowedUser)) {
			return true
		}
	}

	return false
}

// IsAllowedSender 检查结构化 SenderInfo 是否被允许列表许可。
// 它对每个条目调用 identity.MatchAllowed，从而在全部旧格式与
// 新的规范 "platform:id" 格式之间提供统一匹配。
func (c *BaseChannel) IsAllowedSender(sender bus.SenderInfo) bool {
	if len(c.allowList) == 0 {
		return true
	}

	for _, allowed := range c.allowList {
		if allowed == "*" || identity.MatchAllowed(sender, allowed) {
			return true
		}
	}

	return false
}

func (c *BaseChannel) HandleMessageWithContext(
	ctx context.Context,
	deliveryChatID, content string,
	media []string,
	inboundCtx bus.InboundContext,
	senderOpts ...bus.SenderInfo,
) {
	// 可用时基于 SenderInfo 做允许检查，否则回退到字符串检查
	var sender bus.SenderInfo
	if len(senderOpts) > 0 {
		sender = senderOpts[0]
	}
	senderID := strings.TrimSpace(inboundCtx.SenderID)
	if sender.CanonicalID != "" || sender.PlatformID != "" {
		if !c.IsAllowedSender(sender) {
			return
		}
	} else {
		if !c.IsAllowed(senderID) {
			return
		}
	}

	// 若有规范 ID 则将 SenderID 设为规范 ID，否则保留原始 senderID
	resolvedSenderID := senderID
	if sender.CanonicalID != "" {
		resolvedSenderID = sender.CanonicalID
	}

	if resolvedSenderID == "" {
		resolvedSenderID = senderID
	}

	inboundCtx.Channel = c.name
	if inboundCtx.ChatID == "" {
		inboundCtx.ChatID = deliveryChatID
	}
	if inboundCtx.SenderID == "" {
		inboundCtx.SenderID = resolvedSenderID
	}

	scope := BuildMediaScope(c.name, deliveryChatID, inboundCtx.MessageID)

	msg := bus.InboundMessage{
		Context:    inboundCtx,
		Sender:     sender,
		Content:    content,
		Media:      media,
		MediaScope: scope,
	}
	msg = bus.NormalizeInboundMessage(msg)

	// 在发布之前自动触发正在输入指示、消息表情回应与占位消息。
	// 各项能力相互独立——同一条消息可能同时触发这三项。
	// 注意：即使支持流式输出，入站时仍会显示正在输入与占位消息。
	// 若流式输出实际生效，preSend 会跳过占位消息编辑（streamActive map），
	// 并且仍会调用停止正在输入的回调。这样可避免编译期接口检查在流式
	// 输出运行时可能不生效的情况下错误跳过指示器的问题。
	if c.owner != nil && c.placeholderRecorder != nil {
		// 正在输入
		if tc, ok := c.owner.(TypingCapable); ok {
			if stop, err := tc.StartTyping(ctx, deliveryChatID); err == nil {
				c.placeholderRecorder.RecordTypingStop(c.name, deliveryChatID, stop)
			}
		}
		// 表情回应
		if rc, ok := c.owner.(ReactionCapable); ok && msg.MessageID != "" {
			if undo, err := rc.ReactToMessage(ctx, deliveryChatID, msg.MessageID); err == nil {
				c.placeholderRecorder.RecordReactionUndo(c.name, deliveryChatID, undo)
			}
		}
		// 占位消息——独立的处理流水线。
		if pc, ok := c.owner.(PlaceholderCapable); ok {
			if phID, err := pc.SendPlaceholder(ctx, deliveryChatID); err == nil && phID != "" {
				c.placeholderRecorder.RecordPlaceholder(c.name, deliveryChatID, phID)
			}
		}
	}

	if err := c.bus.PublishInbound(ctx, msg); err != nil {
		logger.ErrorCF("channels", "Failed to publish inbound message", map[string]any{
			"channel": c.name,
			"chat_id": deliveryChatID,
			"error":   err.Error(),
		})
	}
}

// HandleInboundContext 仅使用结构化上下文发布一条规范化后的入站消息。
func (c *BaseChannel) HandleInboundContext(
	ctx context.Context,
	deliveryChatID, content string,
	media []string,
	inboundCtx bus.InboundContext,
	senderOpts ...bus.SenderInfo,
) {
	c.HandleMessageWithContext(ctx, deliveryChatID, content, media, inboundCtx, senderOpts...)
}

func (c *BaseChannel) SetRunning(running bool) {
	c.running.Store(running)
}

// SetMediaStore 向渠道注入 MediaStore。
func (c *BaseChannel) SetMediaStore(s media.MediaStore) { c.mediaStore = s }

// GetMediaStore 返回注入的 MediaStore（可能为 nil）。
func (c *BaseChannel) GetMediaStore() media.MediaStore { return c.mediaStore }

// SetPlaceholderRecorder 向渠道注入 PlaceholderRecorder。
func (c *BaseChannel) SetPlaceholderRecorder(r PlaceholderRecorder) {
	c.placeholderRecorder = r
}

// GetPlaceholderRecorder 返回注入的 PlaceholderRecorder（可能为 nil）。
func (c *BaseChannel) GetPlaceholderRecorder() PlaceholderRecorder {
	return c.placeholderRecorder
}

// SetOwner 注入内嵌该 BaseChannel 的具体渠道。
// 这样 HandleMessage 才能自动触发 TypingCapable / ReactionCapable / PlaceholderCapable。
func (c *BaseChannel) SetOwner(ch Channel) {
	c.owner = ch
}

// BuildMediaScope 为媒体生命周期追踪构造 scope key。
func BuildMediaScope(channel, chatID, messageID string) string {
	id := messageID
	if id == "" {
		id = uniqueID()
	}
	return channel + ":" + chatID + ":" + id
}
