package session

import (
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/taskstate"
)

// SessionStore 定义 agent 主循环使用的持久化操作。
// SessionManager（旧版 JSON 后端）和 JSONLBackend 都满足该接口，
// 从而可以在不修改 agent 主循环代码的情况下替换存储层。
//
// 写入方法（Add*、Set*、Truncate*）是发后即忘的：它们不返回错误。
// 实现应在内部记录失败日志。这与 agent 主循环所依赖的原始
// SessionManager 约定一致。
type SessionStore interface {
	// AddMessage 向会话追加一条简单的 role/content 消息。
	AddMessage(sessionKey, role, content string)
	// AddFullMessage 追加一条包含工具调用的完整消息。
	AddFullMessage(sessionKey string, msg providers.Message)
	// GetHistory 返回会话的完整消息历史。
	GetHistory(key string) []providers.Message
	// GetSummary 返回对话摘要，没有时返回 ""。
	GetSummary(key string) string
	// SetSummary 替换对话摘要。
	SetSummary(key, summary string)
	// SetHistory 替换完整消息历史。
	SetHistory(key string, history []providers.Message)
	// TruncateHistory 仅保留最后 keepLast 条消息。
	TruncateHistory(key string, keepLast int)
	// Save 将待持久化状态写入持久化存储。
	Save(key string) error
	// ListSessions 返回所有已知的会话键。
	ListSessions() []string
	// Close 释放存储持有的资源。
	Close() error
}

// TaskStateStore 是一个可选扩展，由能够持久化每个会话的持久任务状态的
// 会话存储实现。agent 主循环和任务工具通过类型断言使用它，
// 以便不支持任务状态的存储仍可正常编译。
type TaskStateStore interface {
	// GetTaskState 返回会话的持久任务状态。
	// 当未存储任务状态时返回（零值状态, false）。
	GetTaskState(sessionKey string) (taskstate.State, bool)
	// SetTaskState 原子地替换会话的持久任务状态。
	SetTaskState(sessionKey string, state taskstate.State) error
}
