package session

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/rlds/chagent/pkg/memory"
	"github.com/rlds/chagent/pkg/providers"
)

// JSONLBackend 将 memory.Store 适配为 SessionStore 接口。
// 写入错误只记录日志而不返回，这与 agent 主循环所依赖的 SessionManager
// “发后即忘”约定保持一致。
type JSONLBackend struct {
	store memory.Store
}

type metaAwareStore interface {
	GetSessionMeta(ctx context.Context, sessionKey string) (memory.SessionMeta, error)
	UpsertSessionMeta(ctx context.Context, sessionKey string, scope json.RawMessage, aliases []string) error
	ResolveSessionKey(ctx context.Context, sessionKey string) (string, bool, error)
}

type aliasPromotingStore interface {
	PromoteAliasHistory(ctx context.Context, sessionKey string, scope json.RawMessage, aliases []string) (bool, error)
}

// MetadataAwareSessionStore 暴露结构化会话元数据操作。
type MetadataAwareSessionStore interface {
	EnsureSessionMetadata(sessionKey string, scope *SessionScope, aliases []string)
	ResolveSessionKey(sessionKey string) string
	GetSessionScope(sessionKey string) *SessionScope
}

// NewJSONLBackend 包装 memory.Store，使其可用作 SessionStore。
func NewJSONLBackend(store memory.Store) *JSONLBackend {
	return &JSONLBackend{store: store}
}

func (b *JSONLBackend) resolveSessionKey(sessionKey string) string {
	metaStore, ok := b.store.(metaAwareStore)
	if !ok {
		return sessionKey
	}
	resolved, found, err := metaStore.ResolveSessionKey(context.Background(), sessionKey)
	if err != nil {
		log.Printf("session: resolve session key: %v", err)
		return sessionKey
	}
	if found && resolved != "" {
		return resolved
	}
	return sessionKey
}

// ResolveSessionKey 在底层存储支持结构化元数据时，将别名映射到其规范会话键。
// 未知别名会回退为原始输入，从而保持现有调用方兼容。
func (b *JSONLBackend) ResolveSessionKey(sessionKey string) string {
	return b.resolveSessionKey(sessionKey)
}

// EnsureSessionMetadata 持久化会话的作用域和别名元数据。
func (b *JSONLBackend) EnsureSessionMetadata(sessionKey string, scope *SessionScope, aliases []string) {
	metaStore, ok := b.store.(metaAwareStore)
	if !ok {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}

	var rawScope json.RawMessage
	if scope != nil {
		data, err := json.Marshal(scope)
		if err != nil {
			log.Printf("session: encode session scope: %v", err)
			return
		}
		rawScope = data
	}
	ctx := context.Background()
	if err := metaStore.UpsertSessionMeta(ctx, sessionKey, rawScope, aliases); err != nil {
		log.Printf("session: upsert session metadata: %v", err)
		return
	}

	if promotingStore, ok := b.store.(aliasPromotingStore); ok {
		if _, err := promotingStore.PromoteAliasHistory(ctx, sessionKey, rawScope, aliases); err != nil {
			log.Printf("session: promote alias history: %v", err)
		}
	}
}

// GetSessionScope 读取会话键或别名对应的结构化作用域元数据。
func (b *JSONLBackend) GetSessionScope(sessionKey string) *SessionScope {
	metaStore, ok := b.store.(metaAwareStore)
	if !ok {
		return nil
	}
	sessionKey = b.resolveSessionKey(sessionKey)
	meta, err := metaStore.GetSessionMeta(context.Background(), sessionKey)
	if err != nil {
		log.Printf("session: get session metadata: %v", err)
		return nil
	}
	if len(meta.Scope) == 0 {
		return nil
	}
	var scope SessionScope
	if err := json.Unmarshal(meta.Scope, &scope); err != nil {
		log.Printf("session: decode session scope: %v", err)
		return nil
	}
	return CloneScope(&scope)
}

func (b *JSONLBackend) AddMessage(sessionKey, role, content string) {
	sessionKey = b.resolveSessionKey(sessionKey)
	if err := b.store.AddMessage(context.Background(), sessionKey, role, content); err != nil {
		log.Printf("session: add message: %v", err)
	}
}

func (b *JSONLBackend) AddFullMessage(sessionKey string, msg providers.Message) {
	sessionKey = b.resolveSessionKey(sessionKey)
	if err := b.store.AddFullMessage(context.Background(), sessionKey, msg); err != nil {
		log.Printf("session: add full message: %v", err)
	}
}

func (b *JSONLBackend) GetHistory(key string) []providers.Message {
	key = b.resolveSessionKey(key)
	msgs, err := b.store.GetHistory(context.Background(), key)
	if err != nil {
		log.Printf("session: get history: %v", err)
		return []providers.Message{}
	}
	return msgs
}

func (b *JSONLBackend) GetSummary(key string) string {
	key = b.resolveSessionKey(key)
	summary, err := b.store.GetSummary(context.Background(), key)
	if err != nil {
		log.Printf("session: get summary: %v", err)
		return ""
	}
	return summary
}

func (b *JSONLBackend) SetSummary(key, summary string) {
	key = b.resolveSessionKey(key)
	if err := b.store.SetSummary(context.Background(), key, summary); err != nil {
		log.Printf("session: set summary: %v", err)
	}
}

func (b *JSONLBackend) SetHistory(key string, history []providers.Message) {
	key = b.resolveSessionKey(key)
	if err := b.store.SetHistory(context.Background(), key, history); err != nil {
		log.Printf("session: set history: %v", err)
	}
}

func (b *JSONLBackend) TruncateHistory(key string, keepLast int) {
	key = b.resolveSessionKey(key)
	if err := b.store.TruncateHistory(context.Background(), key, keepLast); err != nil {
		log.Printf("session: truncate history: %v", err)
	}
}

// Save 持久化会话状态。由于 JSONL 存储对每次写入都会立即 fsync，
// 数据本身已经是持久化的。Save 执行压缩以回收被逻辑截断消息占用的
// 空间（没有此类消息时为空操作）。
func (b *JSONLBackend) Save(key string) error {
	key = b.resolveSessionKey(key)
	return b.store.Compact(context.Background(), key)
}

// Close 释放底层存储持有的资源。
func (b *JSONLBackend) Close() error {
	return b.store.Close()
}

// ListSessions 返回所有已知的会话键。
func (b *JSONLBackend) ListSessions() []string {
	return b.store.ListSessions()
}
