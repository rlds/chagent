// Chagent - 超轻量级个人 AI agent
//
// Agent 循环健壮性故障注入测试（task-28，functest-20261030 R8）。
//
// 目标：用确定性 fake provider / fake tool / fake context manager 在
// runTurn → SetupTurn/CallLLM/ExecuteTools/Finalize 的真实路径上注入故障，
// 锁定以下不变量：
//  1. 故障不得变成 panic（进程级崩溃）；
//  2. 每个 assistant 消息里的 tool_call 必须有对应的 tool-result
//     （“每个 tool_call 必有 tool-result”核心不变量，含中止/异常路径）；
//  3. 故障必须被降级为错误结果、错误返回或显式日志，而不是静默破坏会话历史；
//  4. 事件/持久化后端故障不得阻塞或中断回合。
//
// 其中两个用例对应本轮审查发现并修复的 P1 缺陷（修复前失败）：
//  - TestFaultNilProviderResponseReturnsError（provider 返回 (nil,nil) 时 nil deref）；
//  - TestFaultHookAbortDuringToolExecutionRestoresSession（hook 中止回合留下悬空 tool_call）。

package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/tools"
)

// =============================================================================
// 故障注入夹具与断言辅助
// =============================================================================

// faultRunTurn 驱动真实 runTurn，并返回可继续断言的 turnState。
func faultRunTurn(
	t *testing.T,
	al *AgentLoop,
	agent *AgentInstance,
	sessionKey, userMessage string,
) (*turnState, turnResult, error) {
	t.Helper()
	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = userMessage
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "fault-" + sessionKey,
		context: newTurnContext(nil, nil, nil),
	})
	pipeline := NewPipeline(al)
	result, err := al.runTurn(context.Background(), ts, pipeline)
	return ts, result, err
}

// faultDanglingToolCallIDs 返回会话历史中“有 tool_call 但没有对应 tool-result”
// 的调用 ID 列表（重复 ID 按出现次数配对）。空切片表示不变量成立。
func faultDanglingToolCallIDs(history []providers.Message) []string {
	pending := make(map[string]int)
	order := make([]string, 0, 4)
	for _, msg := range history {
		if msg.Role == "assistant" {
			for _, tc := range msg.ToolCalls {
				pending[tc.ID]++
				order = append(order, tc.ID)
			}
		}
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if pending[msg.ToolCallID] > 0 {
				pending[msg.ToolCallID]--
			}
		}
	}
	var dangling []string
	for _, id := range order {
		if pending[id] > 0 {
			dangling = append(dangling, id)
		}
	}
	return dangling
}

// faultAssertNoDanglingToolCalls 断言核心不变量：历史中没有悬空 tool_call。
func faultAssertNoDanglingToolCalls(t *testing.T, history []providers.Message) {
	t.Helper()
	if dangling := faultDanglingToolCallIDs(history); len(dangling) != 0 {
		t.Fatalf("dangling tool_call(s) without tool-result: %v\nhistory:\n%s",
			dangling, faultDumpHistory(history))
	}
}

// faultAssertHistoryPreserved 断言回合中止后历史与会话起点完全一致。
func faultAssertHistoryPreserved(t *testing.T, before, after []providers.Message) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("session history changed by aborted turn: before=%d after=%d\nbefore:\n%s\nafter:\n%s",
			len(before), len(after), faultDumpHistory(before), faultDumpHistory(after))
	}
	for i := range before {
		if before[i].Role != after[i].Role || before[i].Content != after[i].Content {
			t.Fatalf("history[%d] changed by aborted turn: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

func faultDumpHistory(history []providers.Message) string {
	var b strings.Builder
	for i, msg := range history {
		ids := make([]string, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			ids = append(ids, tc.ID)
		}
		fmt.Fprintf(&b, "  [%d] role=%s tool_calls=[%s] tool_call_id=%q\n",
			i, msg.Role, strings.Join(ids, ","), msg.ToolCallID)
	}
	return b.String()
}

// faultToolResults 返回历史中所有 tool 消息（用于断言错误结果内容）。
func faultToolResults(history []providers.Message) []providers.Message {
	out := make([]providers.Message, 0, 2)
	for _, msg := range history {
		if msg.Role == "tool" {
			out = append(out, msg)
		}
	}
	return out
}

// faultToolSchemaArgs 是空参数工具的合法 schema。
func faultToolSchemaArgs() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// =============================================================================
// 故障工具
// =============================================================================

// faultPanicTool 在执行时 panic。
type faultPanicTool struct{}

func (faultPanicTool) Name() string               { return "fault_panic" }
func (faultPanicTool) Description() string        { return "panics on execute" }
func (faultPanicTool) Parameters() map[string]any { return faultToolSchemaArgs() }
func (faultPanicTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	panic("injected tool panic")
}

// faultNilResultTool 返回 nil ToolResult。
type faultNilResultTool struct{}

func (faultNilResultTool) Name() string               { return "fault_nil" }
func (faultNilResultTool) Description() string        { return "returns nil result" }
func (faultNilResultTool) Parameters() map[string]any { return faultToolSchemaArgs() }
func (faultNilResultTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return nil
}

// faultDeliveryTool 立即交付用户响应（ResponseHandled），使回合不经第二次 LLM 调用即收尾。
type faultDeliveryTool struct{}

func (faultDeliveryTool) Name() string               { return "fault_delivery" }
func (faultDeliveryTool) Description() string        { return "delivers response without follow-up" }
func (faultDeliveryTool) Parameters() map[string]any { return faultToolSchemaArgs() }
func (faultDeliveryTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.NewToolResult("delivered to user").WithResponseHandled()
}

// faultBlockingTool 阻塞到 ctx 取消，用于注入“工具执行中 hard abort”。
type faultBlockingTool struct {
	started chan struct{}
	once    sync.Once
}

func newFaultBlockingTool() *faultBlockingTool {
	return &faultBlockingTool{started: make(chan struct{})}
}

func (t *faultBlockingTool) Name() string               { return "fault_blocking" }
func (t *faultBlockingTool) Description() string        { return "blocks until cancelled" }
func (t *faultBlockingTool) Parameters() map[string]any { return faultToolSchemaArgs() }
func (t *faultBlockingTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return tools.ErrorResult("tool cancelled")
}

// faultEchoTool 是带 text 参数的合法工具，用于非法参数注入。
type faultEchoTool struct{}

func (faultEchoTool) Name() string        { return "fault_echo" }
func (faultEchoTool) Description() string { return "echoes text" }
func (faultEchoTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
	}
}
func (faultEchoTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	text, _ := args["text"].(string)
	return tools.SilentResult(text)
}

// =============================================================================
// 故障 provider / context manager
// =============================================================================

// faultLLMStep 是脚本化 provider 的单步响应。
type faultLLMStep struct {
	content      string
	toolCalls    []providers.ToolCall
	usage        *providers.UsageInfo
	finishReason string
}

// faultScriptedProvider 按脚本回放响应；脚本耗尽后重复最后一步。
// 同时记录每次请求的 messages，供断言下一次请求内容（本文件自包含，不依赖其他测试文件）。
type faultScriptedProvider struct {
	mu    sync.Mutex
	steps []faultLLMStep
	calls [][]providers.Message
}

func (p *faultScriptedProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	idx := len(p.calls)
	p.calls = append(p.calls, append([]providers.Message(nil), messages...))
	step := faultLLMStep{content: "done", finishReason: "stop"}
	if len(p.steps) > 0 {
		if idx >= len(p.steps) {
			idx = len(p.steps) - 1
		}
		step = p.steps[idx]
	}
	p.mu.Unlock()
	return &providers.LLMResponse{
		Content:      step.content,
		ToolCalls:    step.toolCalls,
		Usage:        step.usage,
		FinishReason: step.finishReason,
	}, nil
}

func (p *faultScriptedProvider) GetDefaultModel() string { return "fault-scripted-model" }

func (p *faultScriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *faultScriptedProvider) callMessages(call int) []providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 0 || call >= len(p.calls) {
		return nil
	}
	return p.calls[call]
}

// faultNilResponseProvider 违反契约返回 (nil, nil)。
type faultNilResponseProvider struct{ calls int }

func (p *faultNilResponseProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	return nil, nil
}

func (p *faultNilResponseProvider) GetDefaultModel() string { return "fault-nil-response" }

// faultCanceledProvider 每次调用都返回 context.Canceled（模拟外部取消而非 hard abort）。
type faultCanceledProvider struct{ calls int }

func (p *faultCanceledProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	return nil, context.Canceled
}

func (p *faultCanceledProvider) GetDefaultModel() string { return "fault-canceled" }

// faultOverflowThenSuccessProvider 第一次返回上下文超限错误，第二次成功。
type faultOverflowThenSuccessProvider struct{ calls int }

func (p *faultOverflowThenSuccessProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return nil, errors.New("context_length_exceeded: prompt is too long")
	}
	return &providers.LLMResponse{Content: "recovered", FinishReason: "stop"}, nil
}

func (p *faultOverflowThenSuccessProvider) GetDefaultModel() string { return "fault-overflow" }

// faultContextManager 包装真实 ContextManager，按配置让 Compact 失败。
type faultContextManager struct {
	inner        ContextManager
	compactFails int
	compacts     int
}

func (m *faultContextManager) Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	return m.inner.Assemble(ctx, req)
}

func (m *faultContextManager) Compact(ctx context.Context, req *CompactRequest) error {
	m.compacts++
	if m.compactFails > 0 {
		m.compactFails--
		return errors.New("injected compact failure")
	}
	return m.inner.Compact(ctx, req)
}

func (m *faultContextManager) Ingest(ctx context.Context, req *IngestRequest) error {
	return m.inner.Ingest(ctx, req)
}

func (m *faultContextManager) Clear(ctx context.Context, sessionKey string) error {
	return m.inner.Clear(ctx, sessionKey)
}

// faultFailingSaveStore 让 Save 失败（模拟磁盘/压缩故障），其余方法委托真实 store。
type faultFailingSaveStore struct {
	session.SessionStore
	err   error
	saves int
}

func (s *faultFailingSaveStore) Save(key string) error {
	s.saves++
	return s.err
}

// faultAbortToolHook 在 BeforeTool 返回 HookActionAbortTurn。
type faultAbortToolHook struct{}

func (faultAbortToolHook) BeforeTool(
	_ context.Context, call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	return call, HookDecision{Action: HookActionAbortTurn, Reason: "injected hook abort"}, nil
}

func (faultAbortToolHook) AfterTool(
	_ context.Context, result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}

// faultNewLoop 创建独立工作区的最小 AgentLoop。
func faultNewLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, *AgentInstance, func()) {
	t.Helper()
	return newHookTestLoop(t, provider)
}

// =============================================================================
// 1) Provider/LLM 失败
// =============================================================================

// TestFaultNilProviderResponseReturnsError 覆盖 P1：provider 返回 (nil, nil)。
//
// 修复前：pipeline_llm.go 在 after-LLM 后处理直接解引用 exec.response.FinishReason
// → nil pointer dereference（worker 里被 recover 吞掉，回合无错误事件/无回复；
// 直接调用 runTurn/runAgentLoop 的嵌入方则进程崩溃）。
// 修复后：视为 provider 契约错误 → emit KindAgentError + 返回错误，会话保持结构一致。
func TestFaultNilProviderResponseReturnsError(t *testing.T) {
	provider := &faultNilResponseProvider{}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	errorCh, closeErrors := subscribeRuntimeEventsForTest(t, al, 16, runtimeevents.KindAgentError)
	defer closeErrors()

	_, _, err := faultRunTurn(t, al, agent, "fault-nil-response", "go")
	if err == nil {
		t.Fatal("nil provider response must surface as an error, not panic or silent success")
	}
	if !strings.Contains(err.Error(), "nil 响应") {
		t.Fatalf("error = %v, want it to mention the nil provider response", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (nil response is not retryable)", provider.calls)
	}

	evt := waitForRuntimeEvent(t, errorCh, 2*time.Second, func(evt runtimeevents.Event) bool {
		return evt.Kind == runtimeevents.KindAgentError
	})
	payload, ok := evt.Payload.(ErrorPayload)
	if !ok {
		t.Fatalf("agent error payload type = %T", evt.Payload)
	}
	if payload.Stage != "llm" {
		t.Fatalf("agent error stage = %q, want llm", payload.Stage)
	}

	// 失败的回合不得写入半成品 assistant 消息，也不得留下悬空 tool_call。
	history := agent.Sessions.GetHistory("fault-nil-response")
	faultAssertNoDanglingToolCalls(t, history)
	for _, msg := range history {
		if msg.Role == "assistant" {
			t.Fatalf("failed turn persisted an assistant message: %+v", msg)
		}
	}
}

// TestFaultProviderCanceledWithoutHardAbortSurfacesError 覆盖“provider 返回
// context.Canceled 但未请求 hard abort”：回合必须以错误收尾，不得 panic、
// 不得写入 assistant 半成品，也不得留下悬空 tool_call。
func TestFaultProviderCanceledWithoutHardAbortSurfacesError(t *testing.T) {
	provider := &faultCanceledProvider{}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	_, _, err := faultRunTurn(t, al, agent, "fault-provider-canceled", "go")
	if err == nil {
		t.Fatal("provider cancellation must surface as a turn error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %v, want context canceled", err)
	}
	history := agent.Sessions.GetHistory("fault-provider-canceled")
	faultAssertNoDanglingToolCalls(t, history)
	for _, msg := range history {
		if msg.Role == "assistant" {
			t.Fatalf("canceled turn persisted an assistant message: %+v", msg)
		}
	}
}

// TestFaultEmptyProviderResponseUsesDefaultResponse 覆盖 non-nil 空响应：
// 空内容/无 tool_calls 的合法响应必须降级为默认回复而不是 panic 或空转。
func TestFaultEmptyProviderResponseUsesDefaultResponse(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{{finishReason: "stop"}}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	result, err := func() (turnResult, error) {
		_, res, runErr := faultRunTurn(t, al, agent, "fault-empty-response", "go")
		return res, runErr
	}()
	if err != nil {
		t.Fatalf("empty provider response must not fail the turn: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("status = %q, want completed", result.status)
	}
	// makeTestProcessOpts 设置的默认回复是回合级配置，而不是全局 defaultResponse。
	wantDefault := makeTestProcessOpts("x").DefaultResponse
	if result.finalContent != wantDefault {
		t.Fatalf("finalContent = %q, want process default response %q", result.finalContent, wantDefault)
	}
}

// TestFaultInvalidProviderUsageIsTolerated 覆盖畸形 usage：
// 负值不得写入用量锚点；极大值必须被钳制；两者都不得 panic。
func TestFaultInvalidProviderUsageIsTolerated(t *testing.T) {
	t.Run("negative usage does not create anchor", func(t *testing.T) {
		provider := &faultScriptedProvider{steps: []faultLLMStep{{
			content: "done", finishReason: "stop",
			usage: &providers.UsageInfo{PromptTokens: -100, CompletionTokens: -5},
		}}}
		al, agent, cleanup := faultNewLoop(t, provider)
		defer cleanup()
		ts, _, err := faultRunTurn(t, al, agent, "fault-usage-negative", "go")
		if err != nil {
			t.Fatalf("negative usage must not fail the turn: %v", err)
		}
		if anchor := ts.usageAnchorSnapshot(); anchor != nil {
			t.Fatalf("negative usage recorded an anchor: %+v", anchor)
		}
	})

	t.Run("oversized usage is clamped", func(t *testing.T) {
		provider := &faultScriptedProvider{steps: []faultLLMStep{{
			content: "done", finishReason: "stop",
			usage: &providers.UsageInfo{PromptTokens: 1 << 40, TotalTokens: 1 << 41},
		}}}
		al, agent, cleanup := faultNewLoop(t, provider)
		defer cleanup()
		ts, _, err := faultRunTurn(t, al, agent, "fault-usage-huge", "go")
		if err != nil {
			t.Fatalf("oversized usage must not fail the turn: %v", err)
		}
		anchor := ts.usageAnchorSnapshot()
		if anchor == nil {
			t.Fatal("oversized but positive usage should still record an anchor")
		}
		if ratio := anchor.correctionRatio(); ratio < 0.25 || ratio > 4 {
			t.Fatalf("correctionRatio = %v, want clamped to [0.25, 4]", ratio)
		}
	})
}

// TestFaultContextOverflowRetrySurvivesCompactionFailure 覆盖上下文超限重试时
// 压缩后端失败：重试仍必须继续，第二次成功后回合完成。
func TestFaultContextOverflowRetrySurvivesCompactionFailure(t *testing.T) {
	provider := &faultOverflowThenSuccessProvider{}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	cm := &faultContextManager{inner: al.contextManager, compactFails: 1}
	al.contextManager = cm

	result, err := func() (turnResult, error) {
		_, res, runErr := faultRunTurn(t, al, agent, "fault-overflow", "go")
		return res, runErr
	}()
	if err != nil {
		t.Fatalf("context overflow retry must recover after one compaction failure: %v", err)
	}
	if result.status != TurnEndStatusCompleted || result.finalContent != "recovered" {
		t.Fatalf("status=%q finalContent=%q, want completed/recovered", result.status, result.finalContent)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (context retry)", provider.calls)
	}
	if cm.compacts != 1 {
		t.Fatalf("compaction attempts = %d, want 1", cm.compacts)
	}
}

// =============================================================================
// 2) 工具执行故障
// =============================================================================

// TestFaultToolPanicYieldsErrorToolResult 覆盖工具 panic：注册表恢复 panic 并
// 合成错误 tool-result；回合继续并完成，且不变量成立。
func TestFaultToolPanicYieldsErrorToolResult(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "panic-call", Name: "fault_panic", Arguments: map[string]any{}},
		}},
		{content: "final", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultPanicTool{})

	result, err := func() (turnResult, error) {
		_, res, runErr := faultRunTurn(t, al, agent, "fault-tool-panic", "go")
		return res, runErr
	}()
	if err != nil {
		t.Fatalf("tool panic must be absorbed, not fail the turn: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("status = %q, want completed", result.status)
	}
	history := agent.Sessions.GetHistory("fault-tool-panic")
	faultAssertNoDanglingToolCalls(t, history)
	results := faultToolResults(history)
	if len(results) != 1 || !strings.Contains(results[0].Content, "panic") {
		t.Fatalf("tool results = %+v, want one panic error result", results)
	}
}

// TestFaultToolNilResultYieldsErrorToolResult 覆盖工具返回 nil 结果：
// 注册表合成错误结果；不变量成立且回合完成。
func TestFaultToolNilResultYieldsErrorToolResult(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "nil-call", Name: "fault_nil", Arguments: map[string]any{}},
		}},
		{content: "final", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultNilResultTool{})

	if _, _, err := faultRunTurn(t, al, agent, "fault-tool-nil", "go"); err != nil {
		t.Fatalf("nil tool result must be absorbed, not fail the turn: %v", err)
	}
	history := agent.Sessions.GetHistory("fault-tool-nil")
	faultAssertNoDanglingToolCalls(t, history)
	results := faultToolResults(history)
	if len(results) != 1 || !strings.Contains(results[0].Content, "nil 结果") {
		t.Fatalf("tool results = %+v, want one nil-result error", results)
	}
}

// TestFaultUnknownToolYieldsErrorToolResult 覆盖未知工具名：必须合成错误
// tool-result（而不是让 tool_call 悬空）。
func TestFaultUnknownToolYieldsErrorToolResult(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "unknown-call", Name: "definitely_not_registered", Arguments: map[string]any{}},
		}},
		{content: "final", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	if _, _, err := faultRunTurn(t, al, agent, "fault-tool-unknown", "go"); err != nil {
		t.Fatalf("unknown tool must be absorbed, not fail the turn: %v", err)
	}
	history := agent.Sessions.GetHistory("fault-tool-unknown")
	faultAssertNoDanglingToolCalls(t, history)
	results := faultToolResults(history)
	if len(results) != 1 || !strings.Contains(results[0].Content, "不存在") {
		t.Fatalf("tool results = %+v, want one not-found error", results)
	}
}

// TestFaultInvalidToolArgumentsYieldErrorToolResult 覆盖畸形/非法工具参数：
// schema 校验失败必须合成错误 tool-result，不变量成立。
func TestFaultInvalidToolArgumentsYieldErrorToolResult(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "invalid-args-call", Name: "fault_echo", Arguments: map[string]any{"unexpected": true}},
		}},
		{content: "final", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultEchoTool{})

	if _, _, err := faultRunTurn(t, al, agent, "fault-tool-invalid-args", "go"); err != nil {
		t.Fatalf("invalid tool args must be absorbed, not fail the turn: %v", err)
	}
	history := agent.Sessions.GetHistory("fault-tool-invalid-args")
	faultAssertNoDanglingToolCalls(t, history)
	results := faultToolResults(history)
	if len(results) != 1 || !strings.Contains(results[0].Content, "参数无效") {
		t.Fatalf("tool results = %+v, want one invalid-arguments error", results)
	}
}

// =============================================================================
// 3) 取消 / 中止
// =============================================================================

// TestFaultHookAbortDuringToolExecutionRestoresSession 覆盖 P1：
// HookActionAbortTurn 在工具执行中途触发。
//
// 修复前：assistant 消息（带 tool_calls）已落库，但工具结果没有落库，且 hook abort
// 路径不像 hard abort 那样回滚会话 → 会话中出现悬空 tool_call（下一次请求可能被
// provider 以“tool_call 缺少 tool 响应”拒绝）。修复后：回合以错误收尾并回滚到回合起点。
func TestFaultHookAbortDuringToolExecutionRestoresSession(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call twice", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "abort-call-1", Name: "fault_echo", Arguments: map[string]any{"text": "a"}},
			{ID: "abort-call-2", Name: "fault_echo", Arguments: map[string]any{"text": "b"}},
		}},
		{content: "should not be reached", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultEchoTool{})
	if err := al.MountHook(NamedHook("fault-abort-hook", faultAbortToolHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	sessionKey := "fault-hook-abort"
	agent.Sessions.AddMessage(sessionKey, "user", "之前的提问")
	agent.Sessions.AddMessage(sessionKey, "assistant", "之前的回答")
	if err := agent.Sessions.Save(sessionKey); err != nil {
		t.Fatalf("seed save failed: %v", err)
	}
	before := agent.Sessions.GetHistory(sessionKey)

	_, _, err := faultRunTurn(t, al, agent, sessionKey, "触发中止")
	if err == nil {
		t.Fatal("hook abort must surface as a turn error")
	}
	if !strings.Contains(err.Error(), "hook 请求中止回合") {
		t.Fatalf("error = %v, want hook abort error", err)
	}
	after := agent.Sessions.GetHistory(sessionKey)
	faultAssertNoDanglingToolCalls(t, after)
	faultAssertHistoryPreserved(t, before, after)
}

// TestFaultHardAbortBeforeTurnPreservesSessionHistory 覆盖“回合开始前已有 pending
// hard abort（例如排队中的 /stop）”：回合以 aborted 收尾、不调用 LLM，且历史不被破坏。
func TestFaultHardAbortBeforeTurnPreservesSessionHistory(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{{content: "must not run", finishReason: "stop"}}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()

	sessionKey := "fault-hard-abort-before"
	agent.Sessions.AddMessage(sessionKey, "user", "旧问题")
	agent.Sessions.AddMessage(sessionKey, "assistant", "旧回答")
	if err := agent.Sessions.Save(sessionKey); err != nil {
		t.Fatalf("seed save failed: %v", err)
	}
	before := agent.Sessions.GetHistory(sessionKey)

	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = "新问题"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "fault-hard-abort-before", context: newTurnContext(nil, nil, nil)})
	_ = ts.requestHardAbort()

	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err != nil {
		t.Fatalf("hard abort before turn must not return an error: %v", err)
	}
	if result.status != TurnEndStatusAborted {
		t.Fatalf("status = %q, want aborted", result.status)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 (turn aborted before setup)", got)
	}
	faultAssertHistoryPreserved(t, before, agent.Sessions.GetHistory(sessionKey))
}

// TestFaultHardAbortDuringToolRestoresSession 覆盖工具执行中 hard abort：
// 回合 aborted、会话回滚到回合起点、不变量成立、无 goroutine 阻塞。
func TestFaultHardAbortDuringToolRestoresSession(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "abort-slow-call", Name: "fault_blocking", Arguments: map[string]any{}},
		}},
		{content: "must not run", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	blocking := newFaultBlockingTool()
	al.RegisterTool(blocking)

	sessionKey := "fault-hard-abort-during-tool"
	agent.Sessions.AddMessage(sessionKey, "user", "旧问题")
	agent.Sessions.AddMessage(sessionKey, "assistant", "旧回答")
	if err := agent.Sessions.Save(sessionKey); err != nil {
		t.Fatalf("seed save failed: %v", err)
	}
	before := agent.Sessions.GetHistory(sessionKey)

	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = "长工具调用"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "fault-abort-during-tool", context: newTurnContext(nil, nil, nil)})

	type outcome struct {
		result turnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, runErr := al.runTurn(context.Background(), ts, NewPipeline(al))
		done <- outcome{result: res, err: runErr}
	}()

	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}
	ts.requestHardAbort()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not finish after hard abort (possible goroutine/channel leak)")
	}
	if got.err != nil {
		t.Fatalf("hard abort must not return an error: %v", got.err)
	}
	if got.result.status != TurnEndStatusAborted {
		t.Fatalf("status = %q, want aborted", got.result.status)
	}
	after := agent.Sessions.GetHistory(sessionKey)
	faultAssertNoDanglingToolCalls(t, after)
	faultAssertHistoryPreserved(t, before, after)
}

// =============================================================================
// 4) 事件 / 持久化后端故障
// =============================================================================

// TestFaultClosedEventBusAndStalledSubscriberDoNotBlockTurn 覆盖：
//   - 消息总线已关闭（PublishOutbound 返回 ErrBusClosed）；
//   - runtime 事件订阅者永不消费（PublishNonBlocking 必须丢弃而不是阻塞）。
//
// 两种后端故障下回合都必须正常完成。
func TestFaultClosedEventBusAndStalledSubscriberDoNotBlockTurn(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{{content: "done", finishReason: "stop"}}}
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	defer al.Close()

	// 订阅一个永不读取的订阅者：buffer=1，事件多到必然溢出。
	stalled, closeStalled := subscribeRuntimeEventsForTest(t, al, 1)
	defer closeStalled()
	_ = stalled

	// 关闭消息总线：之后任何 PublishOutbound 都返回 ErrBusClosed（错误被忽略）。
	msgBus.Close()

	agent := al.GetRegistry().GetDefaultAgent()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
			SessionKey:      "fault-closed-bus",
			Channel:         "cli",
			ChatID:          "direct",
			UserMessage:     "go",
			DefaultResponse: defaultResponse,
			SendResponse:    false,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closed event bus must not fail the turn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("turn blocked by closed event bus or stalled subscriber")
	}
}

// TestFaultSessionSaveFailureDuringToolDeliveryDegradesGracefully 覆盖工具交付路径
// 的 Save 失败：回合以 completed 收尾（JSONL 存储本身已同步追加，Save 失败等价于
// 压缩失败），只记录 warn，不 panic、不返回错误。
func TestFaultSessionSaveFailureDuringToolDeliveryDegradesGracefully(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "deliver", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "delivery-call", Name: "fault_delivery", Arguments: map[string]any{}},
		}},
		{content: "must not run", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultDeliveryTool{})

	failing := &faultFailingSaveStore{SessionStore: agent.Sessions, err: errors.New("injected disk failure")}
	agent.Sessions = failing

	result, err := func() (turnResult, error) {
		_, res, runErr := faultRunTurn(t, al, agent, "fault-save-failure", "go")
		return res, runErr
	}()
	if err != nil {
		t.Fatalf("save failure during tool delivery must degrade gracefully, got error: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("status = %q, want completed", result.status)
	}
	if failing.saves == 0 {
		t.Fatal("expected the failing Save to be attempted at least once")
	}
	faultAssertNoDanglingToolCalls(t, failing.GetHistory("fault-save-failure"))
}

// =============================================================================
// 5) 迭代上限边界
// =============================================================================

// TestFaultMaxIterationsOneWithToolCallKeepsInvariant 覆盖 MaxIterations=1 +
// 工具调用：回合以 tool-limit 文案收尾，已有工具调用全部有结果，不变量成立。
func TestFaultMaxIterationsOneWithToolCallKeepsInvariant(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "limit-call", Name: "fault_echo", Arguments: map[string]any{"text": "x"}},
		}},
		{content: "must not run", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultEchoTool{})
	agent.MaxIterations = 1

	result, err := func() (turnResult, error) {
		_, res, runErr := faultRunTurn(t, al, agent, "fault-max-iterations", "go")
		return res, runErr
	}()
	if err != nil {
		t.Fatalf("iteration limit must not return an error: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("status = %q, want completed", result.status)
	}
	if result.finalContent != toolLimitResponse {
		t.Fatalf("finalContent = %q, want tool-limit response", result.finalContent)
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (MaxIterations=1)", got)
	}
	history := agent.Sessions.GetHistory("fault-max-iterations")
	faultAssertNoDanglingToolCalls(t, history)
	if results := faultToolResults(history); len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
}

// =============================================================================
// 6) 上下文压力计算边界
// =============================================================================

// TestFaultContextPressureBoundaryInputs 覆盖上下文压力/用量锚点计算的退化输入：
// nil agent、零/负窗口、极大窗口、nil usage、空消息，都不得 panic。
func TestFaultContextPressureBoundaryInputs(t *testing.T) {
	policy := DefaultContextPressurePolicy()

	if got := contextPromptBudget(nil, policy); got != 0 {
		t.Fatalf("budget(nil agent) = %d, want 0", got)
	}
	for _, window := range []int{0, -1, -1 << 20} {
		agent := &AgentInstance{ContextWindow: window, MaxTokens: 4096}
		if got := contextPromptBudget(agent, policy); got != 0 {
			t.Fatalf("budget(window=%d) = %d, want 0", window, got)
		}
	}
	// 极大窗口：浮点乘法的 int 转换不得产生 panic；预算必须仍为正数。
	huge := &AgentInstance{ContextWindow: math.MaxInt, MaxTokens: 4096}
	if got := contextPromptBudget(huge, policy); got < 1 {
		t.Fatalf("budget(huge window) = %d, want >= 1", got)
	}

	nilAgentTS := &turnState{}
	if notice := nilAgentTS.maybeContextPressureNotice(nil, nil); notice != nil {
		t.Fatalf("notice with nil agent = %+v, want nil", notice)
	}
	nilAgentTS.recordUsageAnchor(nil, []providers.Message{{Role: "user", Content: "hi"}}, nil)
	if anchor := nilAgentTS.usageAnchorSnapshot(); anchor != nil {
		t.Fatalf("nil usage recorded anchor: %+v", anchor)
	}

	ts := &turnState{agent: &AgentInstance{ContextWindow: 1000, MaxTokens: 10}}
	if notice := ts.maybeContextPressureNotice(nil, nil); notice != nil {
		t.Fatalf("notice with empty messages = %+v, want nil", notice)
	}
}

// =============================================================================
// 7) 工具参数不可序列化（畸形 tool_call）
// =============================================================================

// TestFaultNonSerializableToolArgumentsDoNotPanic 覆盖 provider 返回不可 JSON
// 序列化的工具参数（例如 chan）：构造 assistant 消息时 Marshal 失败必须降级，
// 工具校验失败产生错误结果，回合不 panic 且不变量成立。
func TestFaultNonSerializableToolArgumentsDoNotPanic(t *testing.T) {
	provider := &faultScriptedProvider{steps: []faultLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{
			{ID: "bad-args-call", Name: "fault_echo", Arguments: map[string]any{"text": make(chan int)}},
		}},
		{content: "final", finishReason: "stop"},
	}}
	al, agent, cleanup := faultNewLoop(t, provider)
	defer cleanup()
	al.RegisterTool(faultEchoTool{})

	if _, _, err := faultRunTurn(t, al, agent, "fault-bad-args", "go"); err != nil {
		t.Fatalf("non-serializable tool args must be absorbed, not fail the turn: %v", err)
	}
	history := agent.Sessions.GetHistory("fault-bad-args")
	faultAssertNoDanglingToolCalls(t, history)
	results := faultToolResults(history)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1 error result", len(results))
	}
}
