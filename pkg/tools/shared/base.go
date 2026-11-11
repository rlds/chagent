package toolshared

import (
	"context"

	"github.com/rlds/chagent/pkg/session"
)

// Tool 是所有工具都必须实现的接口。
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args map[string]any) *ToolResult
}

const (
	ToolPromptLayerCapability = "capability"
	ToolPromptSlotTooling     = "tooling"
	ToolPromptSlotMCP         = "mcp"
	ToolPromptSourceRegistry  = "tool_registry:native"
	ToolPromptSourceDiscovery = "tool_registry:discovery"
)

type PromptMetadata struct {
	Layer  string
	Slot   string
	Source string
}

type PromptMetadataProvider interface {
	PromptMetadata() PromptMetadata
}

// --- 请求作用域的工具上下文（channel / chatID）---
//
// 通过 context.Value 传递，这样并发的工具调用各自拿到
// 自己的不可变副本——单例工具实例上不保留可变状态。
//
// 键是未导出的指针类型变量——保证不会冲突，
// 并且只能通过下面的辅助函数访问。

type toolCtxKey struct{ name string }

var (
	ctxKeyChannel          = &toolCtxKey{"channel"}
	ctxKeyChatID           = &toolCtxKey{"chatID"}
	ctxKeyMessageID        = &toolCtxKey{"messageID"}
	ctxKeyReplyToMessageID = &toolCtxKey{"replyToMessageID"}
	ctxKeyAgentID          = &toolCtxKey{"agentID"}
	ctxKeySessionKey       = &toolCtxKey{"sessionKey"}
	ctxKeySessionScope     = &toolCtxKey{"sessionScope"}
)

// WithToolContext 返回一个携带 channel 和 chatID 的子 context。
func WithToolContext(ctx context.Context, channel, chatID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChannel, channel)
	ctx = context.WithValue(ctx, ctxKeyChatID, chatID)
	return ctx
}

// WithToolMessageContext 返回一个携带入站消息 ID 的子 context。
func WithToolMessageContext(ctx context.Context, messageID, replyToMessageID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyMessageID, messageID)
	ctx = context.WithValue(ctx, ctxKeyReplyToMessageID, replyToMessageID)
	return ctx
}

// WithToolInboundContext 返回一个携带 channel/chat 和入站 ID 的子 context。
func WithToolInboundContext(
	ctx context.Context,
	channel, chatID, messageID, replyToMessageID string,
) context.Context {
	ctx = WithToolContext(ctx, channel, chatID)
	ctx = WithToolMessageContext(ctx, messageID, replyToMessageID)
	return ctx
}

// WithToolSessionContext 返回一个携带轮次级会话元数据的子 context。
func WithToolSessionContext(
	ctx context.Context,
	agentID, sessionKey string,
	scope *session.SessionScope,
) context.Context {
	ctx = context.WithValue(ctx, ctxKeyAgentID, agentID)
	ctx = context.WithValue(ctx, ctxKeySessionKey, sessionKey)
	ctx = context.WithValue(ctx, ctxKeySessionScope, session.CloneScope(scope))
	return ctx
}

// ToolChannel 从 ctx 中取出 channel，未设置时返回 ""。
func ToolChannel(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyChannel).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolChatID 从 ctx 中取出 chatID，未设置时返回 ""。
func ToolChatID(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyChatID).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolMessageID 从 ctx 中取出当前入站消息 ID，未设置时返回 ""。
func ToolMessageID(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyMessageID).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolReplyToMessageID 从 ctx 中取出当前入站消息的回复目标，未设置时返回 ""。
func ToolReplyToMessageID(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyReplyToMessageID).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolAgentID 从 ctx 中取出当前轮次的 agent ID，未设置时返回 ""。
func ToolAgentID(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeyAgentID).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolSessionKey 从 ctx 中取出当前轮次的会话键，未设置时返回 ""。
func ToolSessionKey(ctx context.Context) string {
	v, ok := ctx.Value(ctxKeySessionKey).(string)
	if !ok {
		return ""
	}
	return v
}

// ToolSessionScope 从 ctx 中取出当前轮次的结构化会话作用域。
func ToolSessionScope(ctx context.Context) *session.SessionScope {
	scope, ok := ctx.Value(ctxKeySessionScope).(*session.SessionScope)
	if !ok {
		return nil
	}
	return session.CloneScope(scope)
}

// AsyncCallback 是异步工具用来通知完成的函数类型。
// 异步工具完成工作后，会用结果调用这个回调。
//
// ctx 参数允许在 agent 正在关闭时取消回调。
// result 参数包含工具的执行结果。
type AsyncCallback func(ctx context.Context, result *ToolResult)

// AsyncExecutor 是工具可选实现的接口，用于支持带完成回调的
// 异步执行。
//
// 与旧的 AsyncTool 模式（SetCallback + Execute）不同，AsyncExecutor
// 通过 ExecuteAsync 的参数接收回调。这样消除了并发调用在共享工具
// 实例上互相覆盖回调的数据竞争。
//
// 这适用于：
//   - 不应阻塞 agent 循环的长时间运行操作
//   - 会独立完成的 subagent 派生
//   - 需要稍后报告结果的后台任务
//
// 示例：
//
//	func (t *SpawnTool) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
//	    go func() {
//	        result := t.runSubagent(ctx, args)
//	        if cb != nil { cb(ctx, result) }
//	    }()
//	    return AsyncResult("Subagent spawned, will report back")
//	}
type AsyncExecutor interface {
	Tool
	// ExecuteAsync 异步运行该工具。异步操作完成时，回调 cb 会被
	// 调用（可能来自另一个 goroutine）。调用方（registry）保证
	// cb 非 nil。
	ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult
}

func ToolToSchema(tool Tool) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"parameters":  tool.Parameters(),
		},
	}
}
