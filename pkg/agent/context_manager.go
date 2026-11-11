package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rlds/chagent/pkg/providers"
)

// ContextManager 通过可插拔的策略管理对话上下文。
// 每个 AgentLoop 同时只有一个 ContextManager 生效，由配置选择。
// 默认实现（"legacy"）保留现有的摘要行为。
type ContextManager interface {
	// Assemble 依据 ContextManager 自身的存储构建带预算的上下文。
	// 在 BuildMessages 之前调用，返回可直接交给 LLM 的组装后消息。
	Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error)

	// Compact 压缩对话历史。
	// 在回合结束后调用（内部可能是异步的），并在上下文溢出时同步调用。
	Compact(ctx context.Context, req *CompactRequest) error

	// Ingest 把一条消息记录到 ContextManager 自身的存储中。
	// 在每条消息持久化到会话 JSONL 之后调用。
	Ingest(ctx context.Context, req *IngestRequest) error

	// Clear 清除某个会话的全部已存上下文（消息、摘要等）。
	// 在用户发出 /clear 或 /reset 时调用。
	Clear(ctx context.Context, sessionKey string) error
}

// AssembleRequest 是 Assemble 的输入。
type AssembleRequest struct {
	SessionKey string // 会话标识
	Budget     int    // 上下文窗口（token 数）
	MaxTokens  int    // 最大回复 token 数
}

// AssembleResponse 是 Assemble 的输出。
type AssembleResponse struct {
	History []providers.Message // 供 BuildMessages 使用的组装后对话历史
	Summary string              // 由 BuildMessages 嵌入系统提示的对话摘要
}

// CompactRequest 是 Compact 的输入。
type CompactRequest struct {
	SessionKey string                // 会话标识
	Reason     ContextCompressReason // proactive_budget | llm_retry | summarize
	Budget     int                   // 上下文窗口预算（用于重试时的激进压缩）
}

// IngestRequest 是 Ingest 的输入。
type IngestRequest struct {
	SessionKey string            // 会话标识
	Message    providers.Message // 刚刚持久化的消息
}

// ContextManagerFactory 依据配置构造 ContextManager。
// al 提供对 AgentLoop 运行时资源（provider、model、workspace 等）的访问。
// cfg 是来自 config.json 的原始 JSON 配置（可能为 nil）。
type ContextManagerFactory func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error)

var (
	cmRegistryMu sync.RWMutex
	cmRegistry   = map[string]ContextManagerFactory{}
)

// RegisterContextManager 注册一个具名的 ContextManager 工厂。
func RegisterContextManager(name string, factory ContextManagerFactory) error {
	if name == "" {
		return fmt.Errorf("context manager name is required")
	}
	if factory == nil {
		return fmt.Errorf("context manager %q factory is nil", name)
	}

	cmRegistryMu.Lock()
	defer cmRegistryMu.Unlock()

	if _, exists := cmRegistry[name]; exists {
		return fmt.Errorf("context manager %q is already registered", name)
	}
	cmRegistry[name] = factory
	return nil
}

func lookupContextManager(name string) (ContextManagerFactory, bool) {
	cmRegistryMu.RLock()
	defer cmRegistryMu.RUnlock()

	f, ok := cmRegistry[name]
	return f, ok
}
