package channels

import (
	"context"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
)

// TypingCapable —— 能够显示输入中/思考中指示器的通道。
// StartTyping 启动指示器并返回一个停止函数。
// 停止函数必须幂等，且可安全地多次调用。
type TypingCapable interface {
	StartTyping(ctx context.Context, chatID string) (stop func(), err error)
}

// MessageEditor —— 能够编辑已有消息的通道。
// messageID 始终为 string；各通道在内部自行转换平台特定类型。
type MessageEditor interface {
	EditMessage(ctx context.Context, chatID string, messageID string, content string) error
}

// MessageEditorWithPayload 扩展了 MessageEditor，面向除纯文本内容外
// 还能更新结构化消息元数据的通道。
type MessageEditorWithPayload interface {
	EditMessageWithPayload(
		ctx context.Context,
		chatID string,
		messageID string,
		payload map[string]any,
	) error
}

// MessageDeleter —— 能够按 ID 删除消息的通道。
type MessageDeleter interface {
	DeleteMessage(ctx context.Context, chatID string, messageID string) error
}

// ReactionCapable —— 能够对入站消息添加回应（例如 👀）的通道。
// ReactToMessage 添加回应并返回一个用于移除该回应的撤销函数。
// 撤销函数必须幂等，且可安全地多次调用。
type ReactionCapable interface {
	ReactToMessage(ctx context.Context, chatID, messageID string) (undo func(), err error)
}

// PlaceholderCapable —— 能够发送占位消息（例如 "Thinking... 💭"）的通道，
// 该消息稍后会被编辑为实际响应。
// 通道必须同时实现 MessageEditor，占位消息才有意义。
// SendPlaceholder 返回占位消息在平台上的消息 ID，以便
// Manager.preSend 之后通过 MessageEditor.EditMessage 编辑它。
type PlaceholderCapable interface {
	SendPlaceholder(ctx context.Context, chatID string) (messageID string, err error)
}

// StreamingCapable —— 能够实时展示 LLM 部分输出的通道。
// 若平台拒绝流式输出（例如未开启论坛模式的 Telegram 机器人），
// 通道应优雅降级。此时 Update 变为空操作，
// Finalize 仍会投递最终消息。
type StreamingCapable interface {
	BeginStream(ctx context.Context, chatID string) (Streamer, error)
}

// Streamer 定义在 pkg/bus 中以避免循环导入。
// 该别名使使用 channels.Streamer 的通道实现无需改动。
type Streamer = bus.Streamer

// PlaceholderRecorder 由 Manager 注入到各通道。
// 通道在收到入站消息时调用这些方法登记输入中/占位消息状态。
// Manager 在出站时使用已登记的状态停止输入指示并编辑占位消息。
type PlaceholderRecorder interface {
	RecordPlaceholder(channel, chatID, placeholderID string)
	RecordTypingStop(channel, chatID string, stop func())
	RecordReactionUndo(channel, chatID string, undo func())
}

// CommandRegistrarCapable 由能够向上游平台注册命令菜单的通道实现
// （例如 Telegram BotCommand）。
// 不支持平台级命令菜单的通道可以忽略此接口。
type CommandRegistrarCapable interface {
	RegisterCommands(ctx context.Context, defs []commands.Definition) error
}
