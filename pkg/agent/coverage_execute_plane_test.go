// Chagent - 超轻量级个人 AI agent
//
// pkg/agent 覆盖率提升（task-39 / OPEN-01）：命令处理、hook 管理器、工具执行
// hook 分支、自治恢复、出站发布、实例辅助工具。仅测试。

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
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/taskstate"
	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// agent_outbound.go / agent_steering.go
// ---------------------------------------------------------------------------

func drainOutbound(t *testing.T, al *AgentLoop) []bus.OutboundMessage {
	t.Helper()
	msgBus, ok := al.bus.(*bus.MessageBus)
	if !ok {
		return nil
	}
	var out []bus.OutboundMessage
	for {
		select {
		case msg := <-msgBus.OutboundChan():
			out = append(out, msg)
		default:
			return out
		}
	}
}

func TestCoverOutboundPublishPaths(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()

	// context.Canceled 不发布错误。
	if published := al.maybePublishError(ctx, "cli", "chat", "coverage", context.Canceled); published {
		t.Fatal("context.Canceled 不应发布错误")
	}
	// 普通错误发布 Error processing message。
	if published := al.maybePublishError(ctx, "cli", "chat", "coverage", errors.New("boom")); !published {
		t.Fatal("普通错误应发布")
	}
	outbound := drainOutbound(t, al)
	if len(outbound) == 0 || !strings.Contains(outbound[len(outbound)-1].Content, "处理消息时出错") {
		t.Fatalf("错误发布 outbound = %+v", outbound)
	}

	// publishResponseOrError：err 分支发错误，response 分支发正常回复。
	al.publishResponseOrError(ctx, "cli", "chat", "coverage", "ignored", "", nil, errors.New("fail"))
	if got := drainOutbound(t, al); len(got) == 0 {
		t.Fatal("publishResponseOrError 错误分支未发布")
	}
	al.publishResponseOrError(ctx, "cli", "chat", "coverage", "正常回复", "model", nil, nil)
	outbound = drainOutbound(t, al)
	if len(outbound) == 0 || outbound[len(outbound)-1].Content != "正常回复" {
		t.Fatalf("publishResponseOrError 正常分支 = %+v", outbound)
	}

	// 空回复不发布。
	before := len(drainOutbound(t, al))
	al.PublishResponseIfNeeded(ctx, "cli", "chat", "coverage", "", "", nil)
	if got := len(drainOutbound(t, al)); got != before {
		t.Fatalf("空回复产生了 outbound: %d", got)
	}
}

func TestCoverProcessMessageSyncAndSteering(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()

	// 同步处理普通消息。
	al.processMessageSync(ctx, bus.InboundMessage{
		Channel:    "cli",
		ChatID:     "chat",
		SessionKey: "coverage-sync",
		Content:    "hello",
	})
	if got := drainOutbound(t, al); len(got) == 0 {
		t.Fatal("processMessageSync 未发布回复")
	}

	// runTurnWithSteering：普通消息无 steering。
	al.runTurnWithSteering(ctx, bus.InboundMessage{
		Channel:    "cli",
		ChatID:     "chat",
		SessionKey: "coverage-steering",
		Content:    "hello steering",
	})
	_ = drainOutbound(t, al)

	// system 消息：continuation target 为 nil，回复已由 processSystemMessage 处理。
	al.runTurnWithSteering(ctx, bus.InboundMessage{
		Channel:    "system",
		ChatID:     "telegram:1",
		SessionKey: "coverage-steering-system",
		Content:    "Result:\n后台结果",
	})
}

// ---------------------------------------------------------------------------
// agent_command.go: handleCommand 多命令分支
// ---------------------------------------------------------------------------

func TestCoverHandleCommandBranches(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()

	// 覆盖注册表里所有内置命令的分发路径。
	definitions := al.cmdRegistry.Definitions()
	if len(definitions) == 0 {
		t.Fatal("命令注册表为空")
	}
	handledCount := 0
	for _, def := range definitions {
		command := "/" + def.Name
		opts := makeTestProcessOpts("coverage-command")
		opts.Dispatch.SessionKey = "coverage-command"
		_, handled := al.handleCommand(ctx, bus.InboundMessage{
			Channel:    "cli",
			ChatID:     "chat",
			SessionKey: "coverage-command",
			Content:    command,
		}, agent, &opts)
		if handled {
			handledCount++
		}
	}
	if handledCount == 0 {
		t.Fatal("没有任何内置命令被处理")
	}

	// 子命令路径（取第一个带子命令的定义）。
	for _, def := range definitions {
		if len(def.SubCommands) == 0 {
			continue
		}
		sub := def.SubCommands[0]
		opts := makeTestProcessOpts("coverage-command-sub")
		opts.Dispatch.SessionKey = "coverage-command-sub"
		_, _ = al.handleCommand(ctx, bus.InboundMessage{
			Channel:    "cli",
			ChatID:     "chat",
			SessionKey: "coverage-command-sub",
			Content:    "/" + def.Name + " " + sub.Name,
		}, agent, &opts)
		break
	}

	// 未知命令 / 非命令前缀。
	opts := makeTestProcessOpts("coverage-command")
	opts.Dispatch.SessionKey = "coverage-command"
	if _, handled := al.handleCommand(ctx, bus.InboundMessage{
		Channel: "cli", ChatID: "chat", SessionKey: "coverage-command", Content: "/definitely-unknown",
	}, agent, &opts); handled {
		t.Fatal("未知命令不应被处理")
	}
	if _, handled := al.handleCommand(ctx, bus.InboundMessage{Content: "hello"}, agent, &opts); handled {
		t.Fatal("普通消息不应被当作命令")
	}
}

// ---------------------------------------------------------------------------
// hooks.go: HookManager 生命周期/优先级/审批/观察者
// ---------------------------------------------------------------------------

// coverObserver 实现 RuntimeEventObserver。
type coverObserver struct{ events int }

func (o *coverObserver) OnRuntimeEvent(context.Context, runtimeevents.Event) error {
	o.events++
	return nil
}

func TestCoverHookManagerLifecycle(t *testing.T) {
	events := runtimeevents.NewBus()
	defer events.Close()
	hm := NewHookManager(events.Channel())
	defer hm.Close()

	observer := &coverObserver{}
	if err := hm.Mount(NamedHook("observer", observer)); err != nil {
		t.Fatalf("Mount observer: %v", err)
	}
	// 同名重复挂载：按设计替换而不是报错。
	if err := hm.Mount(NamedHook("observer", observer)); err != nil {
		t.Fatalf("同名 hook 重挂载应替换: %v", err)
	}
	if len(hm.snapshotHooks()) != 1 {
		t.Fatalf("同名 hook 重挂载后数量 = %d, want 1", len(hm.snapshotHooks()))
	}
	if err := hm.Mount(HookRegistration{}); err == nil {
		t.Fatal("空名称 hook 应返回错误")
	}
	if err := hm.Mount(NamedHook("nil-hook", nil)); err == nil {
		t.Fatal("nil hook 应返回错误")
	}

	hook := &coverLLMHook{before: HookActionContinue, after: HookActionContinue}
	if err := hm.Mount(HookRegistration{Name: "interceptor", Priority: 10, Hook: hook}); err != nil {
		t.Fatalf("Mount interceptor: %v", err)
	}

	req := &LLMHookRequest{Model: "m"}
	if _, decision := hm.BeforeLLM(context.Background(), req); decision.normalizedAction() != HookActionContinue {
		t.Fatalf("BeforeLLM decision = %v", decision)
	}
	if _, decision := hm.AfterLLM(context.Background(), &LLMHookResponse{}); decision.normalizedAction() != HookActionContinue {
		t.Fatalf("AfterLLM decision = %v", decision)
	}
	if decision := hm.ApproveTool(context.Background(), &ToolApprovalRequest{Tool: "x"}); !decision.Approved {
		t.Fatal("默认审批应放行")
	}
	hm.Unmount("interceptor")
	hm.Unmount("observer")
	if hooks := hm.snapshotHooks(); len(hooks) != 0 {
		t.Fatalf("Unmount 后 hooks = %+v", hooks)
	}

	// nil manager / nil 参数安全。
	var nilHM *HookManager
	if _, decision := nilHM.BeforeLLM(context.Background(), req); decision.normalizedAction() != HookActionContinue {
		t.Fatal("nil manager BeforeLLM 应为 continue")
	}
}

// ---------------------------------------------------------------------------
// pipeline_execute.go: hook 分支（serial）
// ---------------------------------------------------------------------------

// coverAdjustingToolHook 可在 BeforeTool/AfterTool 返回指定动作。
type coverAdjustingToolHook struct {
	beforeAction   HookAction
	afterAction    HookAction
	afterResult    *tools.ToolResult
	respondUser    string
	respondMedia   []string
	respondHandled bool
}

func (h *coverAdjustingToolHook) BeforeTool(_ context.Context, call *ToolCallHookRequest) (*ToolCallHookRequest, HookDecision, error) {
	if h.beforeAction == HookActionRespond {
		next := call.Clone()
		next.HookResult = &tools.ToolResult{
			ForLLM:          "hook-respond result",
			ForUser:         h.respondUser,
			Media:           h.respondMedia,
			ResponseHandled: h.respondHandled,
		}
		return next, HookDecision{Action: HookActionRespond}, nil
	}
	return call, HookDecision{Action: h.beforeAction}, nil
}

func (h *coverAdjustingToolHook) AfterTool(_ context.Context, resp *ToolResultHookResponse) (*ToolResultHookResponse, HookDecision, error) {
	if h.afterResult != nil || h.afterAction == HookActionModify {
		next := resp.Clone()
		if h.afterResult != nil {
			next.Result = h.afterResult
		}
		return next, HookDecision{Action: HookActionModify}, nil
	}
	return resp, HookDecision{Action: h.afterAction}, nil
}

func runCoverToolTurn(t *testing.T, provider providers.LLMProvider, hook *coverAdjustingToolHook, register func(al *AgentLoop)) error {
	t.Helper()
	al, _, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	if register != nil {
		register(al)
	}
	if hook != nil {
		if err := al.MountHook(NamedHook("cover-tool-hook", hook)); err != nil {
			t.Fatalf("MountHook: %v", err)
		}
	}
	opts := makeTestProcessOpts("coverage-tool-hook")
	opts.Dispatch.SessionKey = "coverage-tool-hook"
	opts.Dispatch.UserMessage = "run tool"
	opts.SendResponse = true
	ts := newTurnState(al.GetRegistry().GetDefaultAgent(), opts, turnEventScope{turnID: "coverage-tool-hook", context: newTurnContext(nil, nil, nil)})
	_, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	return err
}

func TestCoverExecuteToolsHookRespondVariants(t *testing.T) {
	// respond 的 ForUser + SendResponse=true → 向用户发布工具结果。
	err := runCoverToolTurn(t, &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}}, &coverAdjustingToolHook{beforeAction: HookActionRespond, respondUser: "直接给用户的结果"}, func(al *AgentLoop) {
		al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	})
	if err != nil {
		t.Fatalf("respond ForUser turn = %v", err)
	}
}

func TestCoverExecuteToolsAfterToolHookAborts(t *testing.T) {
	// HookActionAbortTurn：返回 hook abort 错误。
	err := runCoverToolTurn(t, &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}}, &coverAdjustingToolHook{afterAction: HookActionAbortTurn}, func(al *AgentLoop) {
		al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	})
	if err == nil {
		t.Fatal("AfterTool HookActionAbortTurn 应返回 hook abort 错误")
	}

	// HookActionHardAbort：走 hard abort 路径（aborted，无错误）。
	al, _, cleanup := newHookTestLoop(t, &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}})
	defer cleanup()
	al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	if err := al.MountHook(NamedHook("cover-tool-hook", &coverAdjustingToolHook{afterAction: HookActionHardAbort})); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	opts := makeTestProcessOpts("coverage-after-hard-abort")
	opts.Dispatch.SessionKey = "coverage-after-hard-abort"
	opts.Dispatch.UserMessage = "run"
	ts := newTurnState(al.GetRegistry().GetDefaultAgent(), opts, turnEventScope{turnID: "coverage-after-hard-abort", context: newTurnContext(nil, nil, nil)})
	result, runErr := al.runTurn(context.Background(), ts, NewPipeline(al))
	if runErr != nil {
		t.Fatalf("AfterTool HardAbort 不应返回错误: %v", runErr)
	}
	if result.status != TurnEndStatusAborted {
		t.Fatalf("AfterTool HardAbort status = %q, want aborted", result.status)
	}
}

func TestCoverExecuteToolsAfterToolModifyNilResult(t *testing.T) {
	// AfterTool 返回 nil Result：代理补 "(hook returned nil tool result)" 错误结果。
	hook := &coverAdjustingToolHook{afterAction: HookActionModify}
	err := runCoverToolTurn(t, &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}}, hook, func(al *AgentLoop) {
		al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	})
	if err != nil {
		t.Fatalf("AfterTool modify nil result turn = %v", err)
	}
}

func TestCoverExecuteToolsHookRespondNilResult(t *testing.T) {
	// HookActionRespond 但没有 HookResult → 记录 warning 并继续真实执行。
	al, _, cleanup := newHookTestLoop(t, &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "cover_echo", Arguments: map[string]any{}}}},
		{content: "done", finishReason: "stop"},
	}})
	defer cleanup()
	al.RegisterTool(&loopGuardCountingTool{name: "cover_echo"})
	if err := al.MountHook(NamedHook("cover-empty-respond", &coverEmptyRespondHook{})); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	opts := makeTestProcessOpts("coverage-empty-respond")
	opts.Dispatch.SessionKey = "coverage-empty-respond"
	opts.Dispatch.UserMessage = "run"
	ts := newTurnState(al.GetRegistry().GetDefaultAgent(), opts, turnEventScope{turnID: "coverage-empty-respond", context: newTurnContext(nil, nil, nil)})
	if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err != nil {
		t.Fatalf("空 respond turn = %v", err)
	}
}

// coverEmptyRespondHook 返回 Respond 动作但不提供 HookResult。
type coverEmptyRespondHook struct{}

func (coverEmptyRespondHook) BeforeTool(_ context.Context, call *ToolCallHookRequest) (*ToolCallHookRequest, HookDecision, error) {
	return call, HookDecision{Action: HookActionRespond}, nil
}
func (coverEmptyRespondHook) AfterTool(_ context.Context, resp *ToolResultHookResponse) (*ToolResultHookResponse, HookDecision, error) {
	return resp, HookDecision{Action: HookActionContinue}, nil
}

// coverDeliveryBatchTool 在批调度中声明并行安全，并直接交付用户响应。
type coverDeliveryBatchTool struct{}

func (coverDeliveryBatchTool) Name() string               { return "cover_delivery" }
func (coverDeliveryBatchTool) Description() string        { return "batch delivery tool" }
func (coverDeliveryBatchTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (coverDeliveryBatchTool) ConcurrencySafe() bool      { return true }
func (coverDeliveryBatchTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.NewToolResult("delivered by batch").WithResponseHandled()
}

func TestCoverExecuteToolsBatchDeliverySummary(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "b1", Name: "cover_delivery", Arguments: map[string]any{}},
			{ID: "b2", Name: "cover_delivery", Arguments: map[string]any{}},
		}},
		{content: "must not run", finishReason: "stop"},
	}}
	al, _, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	al.RegisterTool(coverDeliveryBatchTool{})
	opts := makeTestProcessOpts("coverage-batch-delivery")
	opts.Dispatch.SessionKey = "coverage-batch-delivery"
	opts.Dispatch.UserMessage = "deliver"
	opts.EnableSummary = true
	ts := newTurnState(al.GetRegistry().GetDefaultAgent(), opts, turnEventScope{turnID: "coverage-batch-delivery", context: newTurnContext(nil, nil, nil)})
	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err != nil {
		t.Fatalf("batch delivery turn = %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("status = %q, want completed", result.status)
	}
	// 工具已交付响应：不应再调用第二次 LLM。
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}
}

// ---------------------------------------------------------------------------
// autonomy_runtime.go: resume + final summary
// ---------------------------------------------------------------------------

func TestCoverAutonomyResumeAndFinalSummary(t *testing.T) {
	cfg := defaultAutonomyConfig()
	cfg.ResumeOnStart = true
	cfg.RoundDelayMS = 5000
	al, _, _ := newAutonomyTestLoop(t, &mockProvider{}, cfg)
	agent, store := autonomyAgentLoop(t, al)
	defer al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)

	sessionKey := autonomySessionKey(agent, "coverage-resume")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "恢复任务", 10))
	if resumed := al.resumeAutonomyTasksOnStart(context.Background()); resumed == 0 {
		t.Fatal("resumeAutonomyTasksOnStart 未恢复活跃任务")
	}
	if !al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("恢复后应存在 pending 自治轮次")
	}

	// 关闭 ResumeOnStart：直接返回 0。
	cfgOff := defaultAutonomyConfig()
	cfgOff.ResumeOnStart = false
	alOff, _, _ := newAutonomyTestLoop(t, &mockProvider{}, cfgOff)
	agentOff, storeOff := autonomyAgentLoop(t, alOff)
	keyOff := autonomySessionKey(agentOff, "coverage-resume-off")
	seedAutonomyState(t, storeOff, keyOff, newAutonomyState(t, "不恢复", 5))
	if resumed := alOff.resumeAutonomyTasksOnStart(context.Background()); resumed != 0 {
		t.Fatalf("ResumeOnStart=false 恢复了 %d 个任务", resumed)
	}

	// 直接调用 publishAutonomyFinalSummary（nil/空内容安全 + 正常发布）。
	al.publishAutonomyFinalSummary(context.Background(), nil, taskstate.State{}, AutonomyRoundReasonUserStop)
	ts := autonomyTurnState(al, agent, sessionKey, "cli", "chat", "")
	al.publishAutonomyFinalSummary(context.Background(), ts, taskstate.State{Objective: "完成", Phase: taskstate.PhaseBlocked}, AutonomyRoundReasonBlocked)
	_ = drainOutbound(t, al)

	// publishAutonomyIntermediate：空内容/无通道安全，有内容时发布。
	al.publishAutonomyIntermediate(context.Background(), ts, "")
	al.publishAutonomyIntermediate(context.Background(), &turnState{}, "无通道")
	al.publishAutonomyIntermediate(context.Background(), ts, "中间进展")
}

// ---------------------------------------------------------------------------
// instance.go / spill.go / agent_media.go 小型辅助
// ---------------------------------------------------------------------------

func TestCoverInstanceAndSpillHelpers(t *testing.T) {
	// initSessionStore：目录位置被普通文件占用 → JSONL 初始化失败回退。
	dir := t.TempDir()
	blocked := filepath.Join(dir, "sessions-file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocked file: %v", err)
	}
	store := initSessionStore(blocked)
	if store == nil {
		t.Fatal("initSessionStore 回退后返回 nil")
	}
	_ = store.Close()

	// compilePatterns：合法与非法模式。
	if patterns := compilePatterns([]string{"a.*", "[", "b"}); len(patterns) == 0 {
		t.Fatal("compilePatterns 未编译任何模式")
	}

	// SpillStore：空目录 Cleanup 与越界路径安全。
	workspace := t.TempDir()
	spill := NewSpillStore(workspace)
	if removed, err := spill.Cleanup(); err != nil || removed != 0 {
		t.Fatalf("空 spill Cleanup = (%d, %v)", removed, err)
	}

	// writeSpillFileAtomic：目标目录不可写 → 错误分支。
	badPath := filepath.Join(blocked, "sub", "file.txt")
	if err := writeSpillFileAtomic(badPath, []byte("data"), 0o600); err == nil {
		t.Fatal("不可写目录应返回错误")
	}

	// buildArtifactTags / resolveMediaRefs 空输入安全。
	if tags := buildArtifactTags(nil, nil); len(tags) != 0 {
		t.Fatalf("buildArtifactTags(nil) = %v", tags)
	}
	if resolved := resolveMediaRefs(nil, nil, 1024, 0); len(resolved) != 0 {
		t.Fatalf("resolveMediaRefs(nil) = %v", resolved)
	}
}

var _ = config.Config{}
var _ = time.Second
