// Chagent - 超轻量级个人 AI agent
//
// pkg/agent 覆盖率提升（task-39 / OPEN-01）：运行时/初始化/命令/自治/设置辅助分支。
// 仅测试；不修改生产代码。

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/taskstate"
	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// agent_command.go: buildCommandsRuntime 的闭包（MCP 列表/工具列表）
// ---------------------------------------------------------------------------

func TestCoverBuildCommandsRuntimeMCPClosures(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()
	opts := makeTestProcessOpts("coverage-commands")

	// 无 MCP 配置：两个闭包都走空/报错分支。
	rt := al.buildCommandsRuntime(ctx, agent, &opts)
	if rt == nil {
		t.Fatal("buildCommandsRuntime 返回 nil")
	}
	if ids := rt.ListAgentIDs(); len(ids) == 0 {
		t.Fatal("ListAgentIDs 为空")
	}
	_ = rt.ListDefinitions()
	if servers := rt.ListMCPServers(ctx); servers != nil {
		t.Fatalf("无 MCP 配置时 ListMCPServers = %+v, want nil", servers)
	}
	if _, err := rt.ListMCPTools(ctx, ""); err == nil || !strings.Contains(err.Error(), "server 名称不能为空") {
		t.Fatalf("空 server name 错误 = %v", err)
	}
	if _, err := rt.ListMCPTools(ctx, "unknown"); err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("未知 server 错误 = %v", err)
	}

	// 配置了但禁用的 MCP server：列表可见、工具列表报 disabled。
	al.cfg.Tools.MCP = config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"Disabled_Server": {Enabled: false},
		},
	}
	rt = al.buildCommandsRuntime(ctx, agent, &opts)
	servers := rt.ListMCPServers(ctx)
	if len(servers) != 1 || servers[0].Name != "Disabled_Server" || servers[0].Enabled {
		t.Fatalf("ListMCPServers = %+v", servers)
	}
	if _, err := rt.ListMCPTools(ctx, "disabled_server"); err == nil || !strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("disabled server 错误 = %v", err)
	}

	// MCP 工具被全局关闭：列表仍返回配置项，工具列表报 integration disabled。
	al.cfg.Tools.MCP.ToolConfig.Enabled = false
	rt = al.buildCommandsRuntime(ctx, agent, &opts)
	if servers := rt.ListMCPServers(ctx); len(servers) != 1 {
		t.Fatalf("MCP 关闭时 ListMCPServers = %+v, want 1 项", servers)
	}
	if _, err := rt.ListMCPTools(ctx, "disabled_server"); err == nil || !strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("MCP 关闭时工具列表错误 = %v", err)
	}
}

// ---------------------------------------------------------------------------
// agent_message.go: processSystemMessage
// ---------------------------------------------------------------------------

func TestCoverProcessSystemMessage(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()

	if _, err := al.processSystemMessage(ctx, bus.InboundMessage{Channel: "telegram"}); err == nil {
		t.Fatal("非 system 通道应返回错误")
	}

	// 内部通道：只记录日志，不产生用户回复。
	if resp, err := al.processSystemMessage(ctx, bus.InboundMessage{
		Channel:  "system",
		ChatID:   "system:internal",
		SenderID: "subagent",
		Content:  "Task completed.\n\nResult:\n内部结果",
	}); err != nil || resp != "" {
		t.Fatalf("内部通道 system message = (%q, %v), want (\"\", nil)", resp, err)
	}

	// 普通来源通道：走 runAgentLoop 并返回回复。
	resp, err := al.processSystemMessage(ctx, bus.InboundMessage{
		Channel:  "system",
		ChatID:   "telegram:12345",
		SenderID: "subagent",
		Content:  "Task completed.\n\nResult:\n外部结果",
	})
	if err != nil {
		t.Fatalf("普通 system message 失败: %v", err)
	}
	if !strings.Contains(resp, "Mock response") {
		t.Fatalf("system message 回复 = %q", resp)
	}

	// 没有 chatID 分隔符：originChannel 回落到 cli。
	if _, err := al.processSystemMessage(ctx, bus.InboundMessage{
		Channel:  "system",
		ChatID:   "directchat",
		SenderID: "subagent",
		Content:  "no result marker",
	}); err != nil {
		t.Fatalf("无分隔符 chatID 失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// agent_init.go: registerSharedTools 配置矩阵
// ---------------------------------------------------------------------------

func TestCoverRegisterSharedToolsMatrix(t *testing.T) {
	cases := []struct {
		name  string
		apply func(cfg *config.Config)
	}{
		{"web_search", func(cfg *config.Config) {
			cfg.Tools.Web.Enabled = true
			cfg.Tools.Web.PreferNative = false
		}},
		{"web_fetch", func(cfg *config.Config) { cfg.Tools.WebFetch.Enabled = true }},
		{"message_media", func(cfg *config.Config) {
			cfg.Tools.Message.Enabled = true
			cfg.Tools.Message.MediaEnabled = true
		}},
		{"spawn_subagent", func(cfg *config.Config) {
			cfg.Tools.Spawn.Enabled = true
			cfg.Tools.Subagent.Enabled = true
			cfg.Tools.SpawnStatus.Enabled = true
		}},
		{"task_todo", func(cfg *config.Config) {
			cfg.Tools.Task.Enabled = true
			cfg.Tools.Todo.Enabled = true
		}},
		{"skill_load", func(cfg *config.Config) { cfg.Tools.SkillLoad.Enabled = true }},
		{"edit_append", func(cfg *config.Config) {
			cfg.Tools.EditFile.Enabled = true
			cfg.Tools.AppendFile.Enabled = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = t.TempDir()
			tc.apply(cfg)
			al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockProvider{})
			defer al.Close()
			agent := al.GetRegistry().GetDefaultAgent()
			if agent == nil {
				t.Fatal("no default agent")
			}
			if len(agent.Tools.ToProviderDefs()) == 0 {
				t.Fatal("registerSharedTools 未注册任何工具")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pipeline_setup.go: 主动压缩分支
// ---------------------------------------------------------------------------

func TestCoverPipelineSetupProactiveCompression(t *testing.T) {
	provider := &mockProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	// 极小上下文窗口 + 大量历史：SetupTurn 触发 proactive 压缩。
	// AgentInstance 在构造时快照了窗口/输出预算，必须同时调整实例字段。
	al.cfg.Agents.Defaults.ContextWindow = 600
	al.cfg.Agents.Defaults.MaxTokens = 100
	agent.ContextWindow = 600
	agent.MaxTokens = 100
	sessionKey := "coverage-proactive"
	for i := 0; i < 40; i++ {
		agent.Sessions.AddMessage(sessionKey, "user", strings.Repeat("历史内容 ", 20))
		agent.Sessions.AddMessage(sessionKey, "assistant", strings.Repeat("回答 ", 20))
	}
	if err := agent.Sessions.Save(sessionKey); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = "当前问题"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-proactive", context: newTurnContext(nil, nil, nil)})
	exec, err := NewPipeline(al).SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}
	if exec == nil || len(exec.messages) == 0 {
		t.Fatal("proactive 压缩后 SetupTurn 未返回消息")
	}
}

func TestCoverPipelineSetupProactiveCompressionFailure(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	al.cfg.Agents.Defaults.ContextWindow = 600
	al.cfg.Agents.Defaults.MaxTokens = 100
	agent.ContextWindow = 600
	agent.MaxTokens = 100
	cm := &faultContextManager{inner: al.contextManager, compactFails: 1}
	al.contextManager = cm

	sessionKey := "coverage-proactive-fail"
	for i := 0; i < 20; i++ {
		agent.Sessions.AddMessage(sessionKey, "user", strings.Repeat("x", 200))
		agent.Sessions.AddMessage(sessionKey, "assistant", strings.Repeat("y", 200))
	}
	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = "问题"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-proactive-fail", context: newTurnContext(nil, nil, nil)})
	if _, err := NewPipeline(al).SetupTurn(context.Background(), ts); err != nil {
		t.Fatalf("压缩失败不应让 SetupTurn 失败: %v", err)
	}
	if cm.compacts == 0 {
		t.Fatal("预期 compaction 被调用")
	}
}

// ---------------------------------------------------------------------------
// context_legacy.go: Clear 与压缩后会话状态
// ---------------------------------------------------------------------------

func TestCoverLegacyContextManagerClear(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	legacy, ok := al.contextManager.(*legacyContextManager)
	if !ok {
		t.Skipf("context manager 不是 legacy 实现: %T", al.contextManager)
	}
	sessionKey := "coverage-legacy-clear"
	agent.Sessions.AddMessage(sessionKey, "user", "hi")
	if err := legacy.Clear(context.Background(), sessionKey); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	if history := agent.Sessions.GetHistory(sessionKey); len(history) != 0 {
		t.Fatalf("Clear 后历史 = %+v", history)
	}
	if summary := agent.Sessions.GetSummary(sessionKey); summary != "" {
		t.Fatalf("Clear 后摘要 = %q", summary)
	}
}

// ---------------------------------------------------------------------------
// autonomy_runtime.go: maybeScheduleAutonomyRound 守卫分支
// ---------------------------------------------------------------------------

func TestCoverAutonomyMaybeScheduleGuards(t *testing.T) {
	provider := &mockProvider{}
	cfg := defaultAutonomyConfig()
	// 让排队轮次保持 pending，避免 0 延迟立即消费。
	cfg.RoundDelayMS = 5000
	al, _, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)
	sessionKey := autonomySessionKey(agent, "coverage-guards")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "覆盖自治守卫", 10))
	defer al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)

	makeTS := func() *turnState {
		return autonomyTurnState(al, agent, sessionKey, "cli", "chat", "")
	}
	completed := turnResult{status: TurnEndStatusCompleted, finalContent: "done"}

	// 正常活跃状态：会调度下一轮。
	ts := makeTS()
	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, completed)
	if !al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("活跃任务应调度下一自治轮次")
	}
	al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)

	// 自治关闭：直接返回，不调度。
	disabledCfg := defaultAutonomyConfig()
	disabledCfg.Enabled = false
	alDisabled, _, _ := newAutonomyTestLoop(t, &mockProvider{}, disabledCfg)
	agentDisabled, storeDisabled := autonomyAgentLoop(t, alDisabled)
	keyDisabled := autonomySessionKey(agentDisabled, "coverage-disabled")
	seedAutonomyState(t, storeDisabled, keyDisabled, newAutonomyState(t, "disabled", 5))
	tsDisabled := autonomyTurnState(alDisabled, agentDisabled, keyDisabled, "cli", "chat", "")
	alDisabled.maybeScheduleAutonomyRound(context.Background(), agentDisabled, tsDisabled, completed)
	if alDisabled.hasPendingAutonomyRound(keyDisabled) {
		t.Fatal("自治关闭时不应调度轮次")
	}

	// 子回合 / NoHistory 回合：不驱动自治。
	tsDepth := makeTS()
	tsDepth.depth = 1
	al.maybeScheduleAutonomyRound(context.Background(), agent, tsDepth, completed)
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("子回合不应调度自治轮次")
	}
	tsNoHistory := makeTS()
	tsNoHistory.opts.NoHistory = true
	al.maybeScheduleAutonomyRound(context.Background(), agent, tsNoHistory, completed)
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("NoHistory 回合不应调度自治轮次")
	}

	// 中止/错误状态的回合：不续跑。
	for _, status := range []TurnEndStatus{TurnEndStatusAborted, TurnEndStatusError} {
		al.maybeScheduleAutonomyRound(context.Background(), agent, makeTS(), turnResult{status: status})
		if al.hasPendingAutonomyRound(sessionKey) {
			t.Fatalf("status=%q 时不应调度自治轮次", status)
		}
	}

	// 会话存储不实现 taskstate.Store：安全返回。
	originalStore := agent.Sessions
	agent.Sessions = &faultFailingSaveStore{SessionStore: originalStore}
	al.maybeScheduleAutonomyRound(context.Background(), agent, makeTS(), completed)
	agent.Sessions = originalStore
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("非 taskstate.Store 不应调度自治轮次")
	}

	// 非活跃任务状态：结束并（可选）发布最终摘要。
	terminal := newAutonomyState(t, "已完成任务", 5)
	terminal.Phase = taskstate.PhaseComplete
	seedAutonomyState(t, store, sessionKey, terminal)
	al.maybeScheduleAutonomyRound(context.Background(), agent, makeTS(), completed)
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("已完成任务不应调度自治轮次")
	}

	// 活跃回合占用会话：不调度。
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "占用", 5))
	occupied := makeTS()
	al.registerActiveTurn(occupied)
	al.maybeScheduleAutonomyRound(context.Background(), agent, makeTS(), completed)
	al.clearActiveTurn(occupied)
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("会话已被活跃回合占用时不应调度")
	}
}

// ---------------------------------------------------------------------------
// agent.go: Run / Stop / ReloadProviderAndConfig
// ---------------------------------------------------------------------------

func TestCoverAgentRunStopReload(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()

	// Run：已取消的 ctx 立即返回。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := al.Run(ctx); err != nil {
		t.Fatalf("Run(canceled ctx) = %v, want nil", err)
	}

	al.Stop()

	// Reload：参数校验。
	if err := al.ReloadProviderAndConfig(context.Background(), nil, al.GetConfig()); err == nil {
		t.Fatal("nil provider 应返回错误")
	}
	if err := al.ReloadProviderAndConfig(context.Background(), &mockProvider{}, nil); err == nil {
		t.Fatal("nil config 应返回错误")
	}
	// ctx 已取消：注册表构造前返回错误。
	if err := al.ReloadProviderAndConfig(ctx, &mockProvider{}, al.GetConfig()); err == nil {
		t.Fatal("canceled ctx 应返回错误")
	}
	// 正常 reload。
	if err := al.ReloadProviderAndConfig(context.Background(), &mockProvider{}, al.GetConfig()); err != nil {
		t.Fatalf("ReloadProviderAndConfig failed: %v", err)
	}
	if al.GetConfig() == nil {
		t.Fatal("reload 后 config 为 nil")
	}
}

// ---------------------------------------------------------------------------
// tool_batch.go: runToolBatch 边界
// ---------------------------------------------------------------------------

func TestCoverRunToolBatchEdges(t *testing.T) {
	ctx := context.Background()

	if got := runToolBatch(ctx, nil, nil, 0, nil, nil); len(got) != 0 {
		t.Fatalf("空 items outcomes = %+v", got)
	}

	items := []toolBatchItem{{Index: 0, Tool: "t"}, {Index: 1, Tool: "t"}}
	outcomes := runToolBatch(ctx, items, nil, 0, nil, nil)
	if len(outcomes) != 2 || !outcomes[0].Skipped || !outcomes[1].Skipped {
		t.Fatalf("nil execFn outcomes = %+v", outcomes)
	}

	execCalls := 0
	execFn := func(context.Context, int, string, map[string]any) (*tools.ToolResult, error) {
		execCalls++
		return nil, errors.New("boom")
	}
	outcomes = runToolBatch(ctx, items, execFn, 0, nil, nil)
	if len(outcomes) != 2 || execCalls != 2 {
		t.Fatalf("nil isSafe 串行 outcomes=%+v calls=%d", outcomes, execCalls)
	}

	abort := make(chan struct{})
	close(abort)
	outcomes = runToolBatch(ctx, items, execFn, 2, abort, func(string) bool { return true })
	for i, outcome := range outcomes {
		if !outcome.Skipped {
			t.Fatalf("已 abort 的 batch outcomes[%d] = %+v, want skipped", i, outcome)
		}
	}
}

var _ = time.Second
