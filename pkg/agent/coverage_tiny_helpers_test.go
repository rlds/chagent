// Chagent - 超轻量级个人 AI agent
//
// pkg/agent 覆盖率提升（task-39 / OPEN-01）：剩余零覆盖小函数与摘要回退分支。
// 仅测试。

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
)

// ---------------------------------------------------------------------------
// context_legacy.go: summarizeBatch 主路径与回退路径
// ---------------------------------------------------------------------------

func TestCoverLegacySummarizeBatch(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	legacy, ok := al.contextManager.(*legacyContextManager)
	if !ok {
		t.Skipf("context manager 不是 legacy 实现: %T", al.contextManager)
	}
	batch := []providers.Message{
		{Role: "user", Content: "第一条用户消息"},
		{Role: "assistant", Content: "第一条助手回复"},
	}

	// 主路径：provider 返回非空内容 → 直接采用摘要。
	summary, err := legacy.summarizeBatch(context.Background(), agent, batch, "已有摘要")
	if err != nil {
		t.Fatalf("summarizeBatch = %v", err)
	}
	if !strings.Contains(summary, "Mock response") {
		t.Fatalf("summarizeBatch 摘要 = %q", summary)
	}

	// 回退路径：provider 永远返回空内容 → 使用本地截断摘要。
	emptyProvider := &loopFixProvider{steps: []loopFixLLMStep{{content: ""}}}
	al2, agent2, cleanup2 := newHookTestLoop(t, emptyProvider)
	defer cleanup2()
	legacy2 := &legacyContextManager{al: al2}
	fallbackBatch := []providers.Message{
		{Role: "user", Content: strings.Repeat("内容", 300)},
		{Role: "assistant", Content: ""},
	}
	fallback, err := legacy2.summarizeBatch(context.Background(), agent2, fallbackBatch, "")
	if err != nil {
		t.Fatalf("summarizeBatch fallback = %v", err)
	}
	if !strings.Contains(fallback, "对话摘要") {
		t.Fatalf("回退摘要 = %q", fallback)
	}
}

// ---------------------------------------------------------------------------
// agent_inject.go / agent_mcp.go / agent_utils.go / prompt_policy.go 小函数
// ---------------------------------------------------------------------------

func TestCoverTinyHelperFunctions(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()

	// SetReloadFunc。
	called := false
	al.SetReloadFunc(func() error { called = true; return nil })
	if al.reloadFunc == nil {
		t.Fatal("SetReloadFunc 未记录回调")
	}
	_ = al.reloadFunc()
	if !called {
		t.Fatal("reloadFunc 未被调用")
	}

	// recordRegisteredMCPTool：首次创建内层 map，重复调用幂等。
	registered := map[string]map[string]struct{}{}
	recordRegisteredMCPTool(registered, "main", "mcp_x")
	recordRegisteredMCPTool(registered, "main", "mcp_x")
	if len(registered["main"]) != 1 {
		t.Fatalf("recordRegisteredMCPTool = %+v", registered)
	}

	// mapCommandError 两种格式。
	if got := mapCommandError(commands.ExecuteResult{Err: errors.New("boom")}); !strings.Contains(got, "命令执行失败") {
		t.Fatalf("mapCommandError(empty command) = %q", got)
	}
	if got := mapCommandError(commands.ExecuteResult{Command: "status", Err: errors.New("boom")}); !strings.Contains(got, "/status") {
		t.Fatalf("mapCommandError(command) = %q", got)
	}
	if got := commandsUnavailableSkillMessage(); got == "" {
		t.Fatal("commandsUnavailableSkillMessage 为空")
	}

	// buildUseCommandHelp：nil agent / 无技能 / 有技能。
	if got := buildUseCommandHelp(nil); !strings.Contains(got, "用法：/use") {
		t.Fatalf("buildUseCommandHelp(nil) = %q", got)
	}
	if agent.ContextBuilder != nil {
		agent.ContextBuilder.skillsLoader = nil
		if got := buildUseCommandHelp(agent); !strings.Contains(got, "用法：/use") {
			t.Fatalf("buildUseCommandHelp(no skills) = %q", got)
		}
	}

	// policy prompt contributor 的描述符。
	contributor := policyPromptContributor{autonomyEnabled: true}
	if desc := contributor.PromptSource(); desc.ID == "" || desc.Owner == "" {
		t.Fatalf("policy PromptSource = %+v", desc)
	}
	if (policyPromptContributor{}).enabled() {
		t.Fatal("默认 policyPromptContributor 不应启用")
	}
	if !(policyPromptContributor{workspaceContextEnabled: true}).enabled() {
		t.Fatal("workspaceContextEnabled 应启用 contributor")
	}
}

// ---------------------------------------------------------------------------
// subturn.go: AgentLoopFromContext / SpawnSubTurn 守卫
// ---------------------------------------------------------------------------

func TestCoverSubTurnEntryGuards(t *testing.T) {
	if got := AgentLoopFromContext(context.Background()); got != nil {
		t.Fatal("空 context 不应取到 AgentLoop")
	}
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()

	// 有 AgentLoop 但没有 turnState：SpawnSubTurn 返回错误。
	ctx := WithAgentLoop(context.Background(), al)
	if _, err := SpawnSubTurn(ctx, SubTurnConfig{}); err == nil {
		t.Fatal("缺少 turnState 的 SpawnSubTurn 应返回错误")
	}

	// 有 AgentLoop + turnState，但 parent turnState 无 agent：spawnSubTurn 参数校验失败。
	parentTS := &turnState{}
	ctx2 := withTurnState(ctx, parentTS)
	if _, err := spawnSubTurn(ctx2, al, parentTS, SubTurnConfig{}); err == nil {
		t.Fatal("无 agent 的 parent turnState 应返回错误")
	}
	_ = agent
}

// ---------------------------------------------------------------------------
// agent.go: Run 的入站消息路径
// ---------------------------------------------------------------------------

func TestCoverRunConsumesInboundMessage(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- al.Run(ctx) }()

	// 等待 Run 就绪后投递一条消息，再取消。
	time.Sleep(20 * time.Millisecond)
	if err := al.bus.PublishInbound(ctx, bus.InboundMessage{
		Context:    bus.InboundContext{Channel: "cli", ChatID: "chat", ChatType: "direct"},
		Content:    "run loop message",
		SessionKey: "coverage-run-loop",
	}); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在取消后退出")
	}
	_ = drainOutbound(t, al)
}

// ---------------------------------------------------------------------------
// hook_process.go: removePending
// ---------------------------------------------------------------------------

func TestCoverProcessHookRemovePending(t *testing.T) {
	ph := &ProcessHook{pending: make(map[uint64]chan processHookRPCMessage)}
	ch := make(chan processHookRPCMessage, 1)
	ph.pending[7] = ch
	ph.removePending(7)
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("removePending 应关闭 pending channel")
		}
	default:
		t.Fatal("removePending 后 channel 应已关闭")
	}
	if _, ok := ph.pending[7]; ok {
		t.Fatal("removePending 未删除 pending 项")
	}
	ph.removePending(99) // 不存在 id 安全
}

var _ = os.RemoveAll
var _ = filepath.Join
var _ = config.Config{}
var _ = providers.UsageInfo{}
var _ = time.Second

// TestCoverLegacySummarizeSession 覆盖 legacy 摘要会话的完整流程与边界。
func TestCoverLegacySummarizeSession(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	legacy, ok := al.contextManager.(*legacyContextManager)
	if !ok {
		t.Skipf("context manager 不是 legacy 实现: %T", al.contextManager)
	}

	// 历史 <=4：直接返回。
	sessionKey := "coverage-summarize-session"
	agent.Sessions.AddMessage(sessionKey, "user", "hi")
	legacy.summarizeSession(agent, sessionKey)

	// 构造多个回合：safeCut > 0，走摘要写入路径。
	for i := 0; i < 6; i++ {
		agent.Sessions.AddMessage(sessionKey, "user", "用户问题")
		agent.Sessions.AddMessage(sessionKey, "assistant", "助手回答")
	}
	legacy.summarizeSession(agent, sessionKey)
	if summary := agent.Sessions.GetSummary(sessionKey); summary == "" {
		t.Fatal("summarizeSession 未写入摘要")
	}

	// 超过单条消息 token 上限：omitted 分支。
	huge := strings.Repeat("超长内容", 4000)
	sessionKey2 := "coverage-summarize-omitted"
	agent.Sessions.AddMessage(sessionKey2, "user", huge)
	for i := 0; i < 6; i++ {
		agent.Sessions.AddMessage(sessionKey2, "user", "问题")
		agent.Sessions.AddMessage(sessionKey2, "assistant", "回答")
	}
	legacy.summarizeSession(agent, sessionKey2)
	if summary := agent.Sessions.GetSummary(sessionKey2); summary == "" {
		t.Fatal("超长消息场景未写入摘要")
	}
}

// TestCoverExecuteToolsRespondMediaBusPath 覆盖 hook respond 带 media 且无 channelManager
// 时经消息总线发布媒体（pipeline_execute respond 分支）。
func TestCoverExecuteToolsRespondMediaBusPath(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}}
	al, _, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	hook := &coverAdjustingToolHook{
		beforeAction:   HookActionRespond,
		respondMedia:   []string{"media://cover/image.png"},
		respondHandled: true,
	}
	if err := al.MountHook(NamedHook("cover-respond-media", hook)); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	opts := makeTestProcessOpts("coverage-respond-media")
	opts.Dispatch.SessionKey = "coverage-respond-media"
	opts.Dispatch.UserMessage = "run"
	ts := newTurnState(al.GetRegistry().GetDefaultAgent(), opts, turnEventScope{turnID: "coverage-respond-media", context: newTurnContext(nil, nil, nil)})
	if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err != nil {
		t.Fatalf("respond media turn = %v", err)
	}
}

// TestCoverLegacySummarizeSessionNoEmptyBatch 回归线上空摘要请求：
// validMessages 只有一个开头的 user 边界、后面全是 assistant 时，
// 旧的 findNearestUserMessage 会返回 0，导致 part1 为空，摘要请求
// 只剩“对话内容：”前缀。修复后必须至少带一条消息。
func TestCoverLegacySummarizeSessionNoEmptyBatch(t *testing.T) {
	provider := &loopFixProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	legacy, ok := al.contextManager.(*legacyContextManager)
	if !ok {
		t.Skipf("context manager 不是 legacy 实现: %T", al.contextManager)
	}

	sessionKey := "coverage-summarize-no-empty-batch"
	agent.Sessions.AddMessage(sessionKey, "user", "唯一开场用户消息")
	for i := 0; i < 12; i++ {
		agent.Sessions.AddMessage(sessionKey, "assistant", "中间助手回复")
	}
	// 保留窗口（最后 4 条）内再放一个真实 user，保证 safeCut 落在中间。
	agent.Sessions.AddMessage(sessionKey, "user", "结尾用户消息")
	agent.Sessions.AddMessage(sessionKey, "assistant", "结尾助手回复")
	agent.Sessions.AddMessage(sessionKey, "assistant", "结尾助手回复2")
	agent.Sessions.AddMessage(sessionKey, "assistant", "结尾助手回复3")

	legacy.summarizeSession(agent, sessionKey)

	provider.mu.Lock()
	calls := append([][]providers.Message(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("summarizeSession 未发起摘要请求")
	}
	for i, call := range calls {
		if len(call) == 0 {
			continue
		}
		prompt := call[0].Content
		if strings.Contains(prompt, "为以下对话片段生成简明摘要") &&
			!strings.Contains(prompt, "assistant:") &&
			!strings.Contains(prompt, "user:") {
			t.Fatalf("第 %d 次摘要请求没有对话内容: %q", i, prompt)
		}
	}
}
