package memory

import (
	"context"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/taskstate"
)

// Store 定义了持久化会话存储的接口。
// 每个方法都是原子操作 —— 不存在单独的 Save() 调用。
type Store interface {
	// AddMessage 向会话追加一条简单的文本消息。
	AddMessage(ctx context.Context, sessionKey, role, content string) error

	// AddFullMessage 向会话追加一条完整消息（包含工具调用等）。
	AddFullMessage(ctx context.Context, sessionKey string, msg providers.Message) error

	// GetHistory 按插入顺序返回会话的全部消息。
	// 若会话不存在，返回空切片（而非 nil）。
	GetHistory(ctx context.Context, sessionKey string) ([]providers.Message, error)

	// GetSummary 返回会话的对话摘要。
	// 若不存在摘要，返回空字符串。
	GetSummary(ctx context.Context, sessionKey string) (string, error)

	// SetSummary 更新会话的对话摘要。
	SetSummary(ctx context.Context, sessionKey, summary string) error

	// TruncateHistory 从会话中删除除最后 keepLast 条之外的所有消息。
	// 若 keepLast <= 0，则删除全部消息。
	TruncateHistory(ctx context.Context, sessionKey string, keepLast int) error

	// SetHistory 用给定的历史记录替换会话中的全部消息。
	SetHistory(ctx context.Context, sessionKey string, history []providers.Message) error

	// Compact 通过物理删除已被逻辑截断的数据来回收存储空间。
	// 不累积废弃数据的后端可以返回 nil。
	Compact(ctx context.Context, sessionKey string) error

	// GetTaskState 返回会话的持久化任务状态。
	// 当尚未存储任何任务状态时返回 (零值状态, false)。
	GetTaskState(sessionKey string) (taskstate.State, bool)

	// SetTaskState 原子地替换会话的持久化任务状态。
	// 传入零值状态会清除此前存储的任务状态。
	SetTaskState(sessionKey string, state taskstate.State) error

	// ListSessions 返回所有已知的会话键。
	ListSessions() []string

	// Close 释放存储持有的所有资源。
	Close() error
}
