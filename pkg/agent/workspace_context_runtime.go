package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/taskstate"
)

// fileTouchTools 是那些成功调用后可能暴露嵌套工作区说明文件的结构化工具。
// 有意不解析 shell 导航：按照 chess/DSH 的设计，文件系统工具接口是唯一可靠的
// 信号。
var fileTouchTools = map[string]struct{}{
	"read_file":   {},
	"write_file":  {},
	"edit_file":   {},
	"append_file": {},
}

// recordContextTouch 记录一次成功的首方文件工具触碰。调用方传入最终的
// （可能已被 hook 修改的）参数和结果错误标志。
func (ts *turnState) recordContextTouch(toolName string, args map[string]any, isError bool) {
	if ts == nil || isError || ts.agent == nil || ts.agent.WorkspaceContext == nil {
		return
	}
	if _, ok := fileTouchTools[strings.TrimSpace(toolName)]; !ok {
		return
	}
	path, _ := args["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(ts.workspace, path)
	}
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, existing := range ts.contextTouches {
		if existing == path {
			return
		}
	}
	ts.contextTouches = append(ts.contextTouches, path)
}

// consumeContextTouches 返回累积的触碰记录并清空队列。
func (ts *turnState) consumeContextTouches() []string {
	if ts == nil {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.contextTouches) == 0 {
		return nil
	}
	out := append([]string(nil), ts.contextTouches...)
	ts.contextTouches = nil
	return out
}

// persistRuntimeContextMessage 持久化一条上下文消息，使重放、恢复和压缩
// 都能像对待其他 user 角色上下文一样看到它。
func (ts *turnState) persistRuntimeContextMessage(ctx context.Context, al *AgentLoop, msg providers.Message) {
	if ts == nil || ts.agent == nil || ts.agent.Sessions == nil {
		return
	}
	ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
	ts.recordPersistedMessage(msg)
	ts.ingestMessage(ctx, al, msg)
}

// injectWorkspaceContext 向 history 追加至多一条工作区说明基线或增量消息，
// 并将其持久化。失败只记录日志并忽略：上下文发现绝不能中断回合。
func (ts *turnState) injectWorkspaceContext(ctx context.Context, al *AgentLoop, history []providers.Message, touched []string) []providers.Message {
	if ts == nil || ts.agent == nil || ts.agent.WorkspaceContext == nil {
		return history
	}
	result, err := ts.agent.WorkspaceContext.Compose(ctx, history, touched)
	if err != nil {
		logger.WarnCF("agent", "workspace context compose failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       err.Error(),
		})
		return history
	}
	if result.Message == nil {
		return history
	}
	msg := *result.Message
	ts.persistRuntimeContextMessage(ctx, al, msg)
	return append(history, msg)
}

// injectWorkspaceContextDelta 在下一次模型调用前刷新上下文。
func (ts *turnState) injectWorkspaceContextDelta(ctx context.Context, al *AgentLoop, messages []providers.Message) []providers.Message {
	if ts == nil || ts.agent == nil || ts.agent.WorkspaceContext == nil {
		return messages
	}
	touched := ts.consumeContextTouches()
	if len(touched) == 0 {
		return messages
	}
	return ts.injectWorkspaceContext(ctx, al, messages, touched)
}

// injectTaskStateContext 在存在持久 TaskState 且其实质内容发生变化时，
// 追加 P-09 要求的任务状态上下文。仅轮次/时间戳变化不会重新注入
// （P-10 会携带轮次信息）。
func (ts *turnState) injectTaskStateContext(ctx context.Context, al *AgentLoop, history []providers.Message) []providers.Message {
	if ts == nil || ts.agent == nil || ts.agent.Sessions == nil {
		return history
	}
	store, ok := ts.agent.Sessions.(taskstate.Store)
	if !ok {
		return history
	}
	state, ok := store.GetTaskState(ts.sessionKey)
	if !ok {
		return history
	}
	text := RenderTaskState(state)
	if strings.TrimSpace(text) == "" {
		return history
	}
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Context == nil || msg.Context.Kind != TaskStateContextKind {
			continue
		}
		if msg.Content == text {
			return history
		}
		break
	}
	msg := providers.Message{
		Role:    "user",
		Content: text,
		Context: &protocoltypes.ContextMeta{Kind: TaskStateContextKind, Form: "state"},
	}
	ts.persistRuntimeContextMessage(ctx, al, msg)
	return append(history, msg)
}

// injectRuntimeContext 在非时间类事实或刷新间隔桶发生变化时追加 P-08 运行时快照。
// 快照模式让 system prompt 在各回合之间保持稳定。
func (ts *turnState) injectRuntimeContext(ctx context.Context, al *AgentLoop, history []providers.Message) []providers.Message {
	if ts == nil || ts.agent == nil {
		return history
	}
	if strings.ToLower(strings.TrimSpace(ts.agent.RuntimeContextMode)) != "snapshot" {
		return history
	}
	now := time.Now()
	interval := ts.agent.RuntimeContextInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	projectRoot := ""
	if ts.agent.WorkspaceContext != nil {
		projectRoot = ts.agent.WorkspaceContext.ProjectRoot()
	}
	base := RuntimeSnapshotDigest(
		ts.channel, ts.chatID, ts.opts.SenderID,
		ts.opts.SenderDisplayName, ts.workspace, projectRoot,
		ts.agent.RuntimeContextSender,
	)
	digest := fmt.Sprintf("%s@%d", base, now.Truncate(interval).Unix())
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Context == nil || msg.Context.Kind != RuntimeContextKind {
			continue
		}
		if msg.Context.Digest == digest {
			return history
		}
		break
	}
	senderID, senderName := ts.opts.SenderID, ts.opts.SenderDisplayName
	text := BuildRuntimeSnapshot(
		ts.channel, ts.chatID, senderID, senderName, ts.workspace, projectRoot,
		ts.agent.RuntimeContextSender, now,
	)
	msg := providers.Message{
		Role:    "user",
		Content: text,
		Context: &protocoltypes.ContextMeta{
			Kind:     RuntimeContextKind,
			Form:     "state",
			Digest:   digest,
			Sections: []protocoltypes.ContextSection{{Name: "runtime", Text: text}},
		},
	}
	ts.persistRuntimeContextMessage(ctx, al, msg)
	return append(history, msg)
}
