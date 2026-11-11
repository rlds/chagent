// Chagent - 超轻量级个人 AI agent
//
// Agent 循环上下文机制的回合级回归测试（functest-20261030 D1/D2/D4/D5）。
//
// 这些测试刻意驱动真实的 runTurn → SetupTurn/CallLLM/ExecuteTools 路径，
// 而不是直接调用内部函数，从而锁定“机制是否真的接线”而非“库函数是否正确”：
//   - D1：主回合（非 SubTurn）也必须记录 usage 锚点与 LastUsage；
//   - D2：第 2 次迭代的 pre-step 有序表必须按 100/200/300/350/400/500 注入且不重复；
//   - D4：同一回合内多次任务状态注入必须在下一次请求前收敛为最新一条；
//   - D5：首次 LLM 调用（iteration 1）也必须能注入 context-pressure 提醒。

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/taskstate"
	agenttools "github.com/rlds/chagent/pkg/tools"
)

// =============================================================================
// 脚本化 provider：按固定脚本回放响应并记录每次请求的完整 messages。
// =============================================================================

type loopFixLLMStep struct {
	content      string
	toolCalls    []providers.ToolCall
	usage        *providers.UsageInfo
	finishReason string
}

type loopFixProvider struct {
	mu    sync.Mutex
	steps []loopFixLLMStep
	calls [][]providers.Message
}

func (p *loopFixProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	idx := len(p.calls)
	p.calls = append(p.calls, append([]providers.Message(nil), messages...))
	step := loopFixLLMStep{content: "完成"}
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

func (p *loopFixProvider) GetDefaultModel() string { return "loop-fix-model" }

func (p *loopFixProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *loopFixProvider) callMessages(call int) []providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 0 || call >= len(p.calls) {
		return nil
	}
	return p.calls[call]
}

// =============================================================================
// 测试夹具：真实 AgentLoop + 持久任务状态存储 + 工作区指令（含嵌套指令文件）
// =============================================================================

type loopFixFixture struct {
	al        *AgentLoop
	agent     *AgentInstance
	provider  *loopFixProvider
	store     taskstate.Store
	workspace string
	cfg       *config.Config
}

func newLoopFixFixture(t *testing.T, provider *loopFixProvider) *loopFixFixture {
	t.Helper()

	workspace := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		path := filepath.Join(workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	// AGENT.md 是 agent 定义文件（会被工作区指令发现排除）；AGENTS.md 才是指令文件。
	writeFile("AGENT.md", "# Agent\n\n你是一个上下文机制回归测试 agent。\n")
	writeFile("AGENTS.md", "# 根指令\n\n- 始终使用中文回复。\n")
	writeFile("packages/app/AGENTS.md", "# App 指令\n\n- 修改 `packages/app` 时同步更新文档。\n")
	writeFile("packages/app/main.go", "package app\n")
	// 项目根标记：让嵌套指令文件的显示路径稳定为 packages/app/AGENTS.md。
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("create .git marker: %v", err)
	}

	// ContextWindow/MaxTokens 组合让默认压力预算 = min(0.8*24000, 24000-1000) = 19200，
	// 0.85 band = 16320 tokens：测试可用 ~42000 字符的工具结果或大用户消息稳定跨过。
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:           workspace,
				ModelName:           "test-model",
				MaxTokens:           1000,
				ContextWindow:       24000,
				MaxToolIterations:   10,
				RestrictToWorkspace: true,
				WorkspaceContext:    config.WorkspaceContextConfig{Enabled: true},
				RuntimeContext: config.RuntimeContextConfig{
					Mode:                   config.RuntimeContextModeSnapshot,
					RefreshIntervalSeconds: 600,
				},
			},
		},
	}
	cfg.Tools.ReadFile.Enabled = true

	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	t.Cleanup(al.Close)

	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected a default agent")
	}
	store, ok := agent.Sessions.(taskstate.Store)
	if !ok {
		t.Fatal("default agent session store does not implement taskstate.Store")
	}

	return &loopFixFixture{
		al:        al,
		agent:     agent,
		provider:  provider,
		store:     store,
		workspace: workspace,
		cfg:       cfg,
	}
}

// seedTaskState 在回合开始前写入持久任务状态（SetupTurn 会注入第一份 task-state）。
func (f *loopFixFixture) seedTaskState(t *testing.T, sessionKey, objective string) {
	t.Helper()
	state, err := taskstate.New(objective, 10, time.Now())
	if err != nil {
		t.Fatalf("taskstate.New failed: %v", err)
	}
	if err := f.store.SetTaskState(sessionKey, state); err != nil {
		t.Fatalf("SetTaskState failed: %v", err)
	}
}

// runTurn 直接驱动真实的 al.runTurn（与 runAgentLoop 相同的 SetupTurn/CallLLM/ExecuteTools 路径），
// 并返回 turnState 以便断言回合内状态（usage 锚点等）。
func (f *loopFixFixture) runTurn(t *testing.T, sessionKey, userMessage string) (*turnState, turnResult) {
	t.Helper()
	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = userMessage
	ts := newTurnState(f.agent, opts, turnEventScope{
		turnID:  "turn-" + sessionKey,
		context: newTurnContext(nil, nil, nil),
	})
	pipeline := NewPipeline(f.al)
	result, err := f.al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	return ts, result
}

// loopFixLargeToolResult 返回一份约 40KB 的普通文本（不触发 base64 清洗启发式），
// 用于稳定跨过 context-pressure 的 0.85 band。
func loopFixLargeToolResult() string {
	return strings.Repeat("the quick brown fox jumps over the lazy dog. ", 900)
}

// =============================================================================
// 自定义工具：回合内修改持久任务状态 / 返回可控大小结果
// =============================================================================

type loopFixBumpTaskTool struct {
	mu             sync.Mutex
	store          taskstate.Store
	sessionKey     string
	objectiveLabel string
	firstResult    string
	sleepFirstCall time.Duration
	calls          int
}

func (tl *loopFixBumpTaskTool) Name() string { return "loop_fix_bump_task" }

func (tl *loopFixBumpTaskTool) Description() string {
	return "测试用：修改当前会话的持久任务状态"
}

func (tl *loopFixBumpTaskTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (tl *loopFixBumpTaskTool) Execute(_ context.Context, _ map[string]any) *agenttools.ToolResult {
	tl.mu.Lock()
	tl.calls++
	call := tl.calls
	tl.mu.Unlock()

	// 让回合起点与迭代 2 跨过 runtime-snapshot 的刷新区间边界（digest 以秒截断）。
	if call == 1 && tl.sleepFirstCall > 0 {
		time.Sleep(tl.sleepFirstCall)
	}

	state, ok := tl.store.GetTaskState(tl.sessionKey)
	if !ok {
		return agenttools.ErrorResult("任务状态不存在")
	}
	next := state.Clone()
	next.Objective = fmt.Sprintf("%s v%d", tl.objectiveLabel, call)
	next.Evidence = append(next.Evidence, fmt.Sprintf("回合内更新 %d", call))
	if err := tl.store.SetTaskState(tl.sessionKey, next); err != nil {
		return agenttools.ErrorResult(err.Error())
	}
	if call == 1 && tl.firstResult != "" {
		return agenttools.NewToolResult(tl.firstResult)
	}
	return agenttools.NewToolResult("任务状态已更新")
}

// loopFixNoopTool 返回固定小结果，用于制造一次工具迭代而不改变任何上下文。
type loopFixNoopTool struct{}

func (tl *loopFixNoopTool) Name() string { return "loop_fix_noop" }

func (tl *loopFixNoopTool) Description() string { return "测试用：无副作用工具" }

func (tl *loopFixNoopTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (tl *loopFixNoopTool) Execute(context.Context, map[string]any) *agenttools.ToolResult {
	return agenttools.NewToolResult("ok")
}

// =============================================================================
// 自定义 ContextSource：只在第 2 次采集时贡献一条消息，
// 用于在迭代 2 的尾巴里标记 order 400 的位置。
// =============================================================================

type loopFixLateContextSource struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (src *loopFixLateContextSource) ContextSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{ID: "test:loop-fix-order-400", Owner: "test"}
}

func (src *loopFixLateContextSource) ContributeContext(
	_ context.Context,
	_ PromptBuildRequest,
) ([]ContextContribution, error) {
	src.mu.Lock()
	src.calls++
	call := src.calls
	src.mu.Unlock()
	if call < 2 {
		// 回合起点的第一次采集不贡献，保证消息只出现在迭代 2 的尾巴里。
		return nil, nil
	}
	return []ContextContribution{{
		Text:  src.text,
		Order: preStepOrderSkillCatalog,
		Meta: protocoltypes.ContextMeta{
			Kind: "test-order-400",
			Form: protocoltypes.ContextFormMessage,
		},
	}}, nil
}

// =============================================================================
// 断言辅助
// =============================================================================

func loopFixContextMessagesOfKind(messages []providers.Message, kind string) []providers.Message {
	out := make([]providers.Message, 0, 1)
	for _, msg := range messages {
		if msg.Context == nil || msg.Context.Kind != kind {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func loopFixKindsAfter(messages []providers.Message, start int) []string {
	out := make([]string, 0, len(messages)-start)
	for _, msg := range messages[start:] {
		if msg.Context == nil {
			continue
		}
		out = append(out, msg.Context.Kind)
	}
	return out
}

func loopFixFindLastIndex(messages []providers.Message, match func(providers.Message) bool) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if match(messages[i]) {
			return i
		}
	}
	return -1
}

// =============================================================================
// D1：usage 锚点在主回合必须生效
// =============================================================================

// TestCallLLMMainTurnRecordsUsageAnchor 覆盖 D1（P1）。
//
// 根因：turn_coord.go 把 turnState 放进 turnCtx，却把原始 ctx 作为 CallLLM 的第 1 个实参，
// pipeline_llm.go 的 turnStateFromContext(ctx) 在主回合恒为 nil，SetLastUsage /
// SetLastFinishReason / recordUsageAnchor 全部走死路径（只有 SubTurn 生效）。
//
// 断言：真实 runTurn 跑完一次带 Usage 的 LLM 调用后，该回合 turnState 上必须有
// LastUsage 与 usageAnchor；修复前二者均为 nil。
func TestCallLLMMainTurnRecordsUsageAnchor(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{{
		content:      "已完成。",
		finishReason: "stop",
		usage: &providers.UsageInfo{
			PromptTokens:     1234,
			CompletionTokens: 12,
			TotalTokens:      1246,
		},
	}}}
	f := newLoopFixFixture(t, provider)

	ts, result := f.runTurn(t, "loop-fix-usage-anchor", "记录用量锚点")

	// result.usage 来自 exec.response（不经 turnState），用于确认 provider 确实返回了 usage。
	if result.usage == nil || result.usage.PromptTokens != 1234 {
		t.Fatalf("runTurn result usage = %+v, want prompt_tokens=1234", result.usage)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (single LLM call turn)", provider.callCount())
	}

	// D1 回归点：主回合的 turnState 必须记录 LastUsage（SetLastUsage 生效）。
	lastUsage := ts.GetLastUsage()
	if lastUsage == nil {
		t.Fatal("main-turn LastUsage is nil: CallLLM did not see the turn turnState (D1)")
	}
	if lastUsage.PromptTokens != 1234 || lastUsage.CompletionTokens != 12 {
		t.Fatalf("main-turn LastUsage = %+v, want the provider-reported usage", lastUsage)
	}

	// D1 回归点：SetLastFinishReason 同为该代码块内的死路径，必须一并生效。
	if got := ts.GetLastFinishReason(); got != "stop" {
		t.Fatalf("main-turn LastFinishReason = %q, want %q", got, "stop")
	}

	// D1 回归点：用量锚定计量必须记录锚点，且锚点 prompt_tokens 与 provider 一致。
	anchor := ts.usageAnchorSnapshot()
	if anchor == nil {
		t.Fatal("main-turn usageAnchor is nil: recordUsageAnchor never ran on the main turn (D1)")
	}
	if anchor.promptTokens != 1234 {
		t.Fatalf("usageAnchor.promptTokens = %d, want 1234", anchor.promptTokens)
	}
	if anchor.estimatedTokens <= 0 {
		t.Fatalf("usageAnchor.estimatedTokens = %d, want > 0", anchor.estimatedTokens)
	}
}

// =============================================================================
// D2：pre-step 有序表在迭代 2 真实执行且顺序正确
// =============================================================================

// TestTurnPreStepInjectsOrderedContextSourcesAtSecondIteration 覆盖 D2（P1）。
//
// 场景：fake provider 第 1 次返回工具调用（read_file 触发工作区指令增量、
// 三次相同调用触发 loop-guard、任务状态被回合内工具改写），第 2 次返回最终文本。
//
// 断言第 2 次 provider 请求的“迭代 2 尾巴”按 pre-step 顺序表注入：
// 100 workspace → 200 runtime → 300 task-state → 350 context-pressure →
// 400 注册来源 → 500 loop-guard，且状态型上下文无重复（supersede 生效）。
func TestTurnPreStepInjectsOrderedContextSourcesAtSecondIteration(t *testing.T) {
	bumpCall := func(id string) providers.ToolCall {
		return providers.ToolCall{
			ID:        id,
			Type:      "function",
			Name:      "loop_fix_bump_task",
			Arguments: map[string]any{},
		}
	}
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{
			content:      "先读文件并更新任务状态。",
			finishReason: "tool_calls",
			toolCalls: []providers.ToolCall{
				{
					ID:        "call-read",
					Type:      "function",
					Name:      "read_file",
					Arguments: map[string]any{"path": "packages/app/main.go"},
				},
				bumpCall("call-bump-1"),
				bumpCall("call-bump-2"),
				bumpCall("call-bump-3"),
			},
		},
		{content: "上下文已刷新，任务完成。", finishReason: "stop"},
	}}
	f := newLoopFixFixture(t, provider)
	sessionKey := "loop-fix-prestep-order"
	f.seedTaskState(t, sessionKey, "回合起点任务")

	bump := &loopFixBumpTaskTool{
		store:          f.store,
		sessionKey:     sessionKey,
		objectiveLabel: "有序表回合任务",
		// 约 41400 ASCII 字符 ≈ 16560 tokens：让迭代 2 的用量估算跨过 0.85 压力区间，
		// 而迭代 1 的初始上下文仍然远低于阈值（锚点为空时按启发式估算）。
		// 内容必须像正常文本，避免被工具结果的 base64 启发式清洗折叠。
		firstResult:    loopFixLargeToolResult(),
		sleepFirstCall: 1100 * time.Millisecond,
	}
	f.agent.Tools.Register(bump)

	source := &loopFixLateContextSource{text: "<test_order_400>注册来源上下文</test_order_400>"}
	if err := f.agent.ContextBuilder.RegisterContextSource(source); err != nil {
		t.Fatalf("RegisterContextSource failed: %v", err)
	}
	// runtime 快照 digest 以 `now.Truncate(interval).Unix()`（秒）计算：把刷新间隔设为 1s，
	// 并让首个工具调用跨过秒边界，迭代 2 才会重新注入 order 200。
	f.agent.RuntimeContextInterval = time.Second

	_, result := f.runTurn(t, sessionKey, "执行有序表回合")
	if result.status != TurnEndStatusCompleted {
		t.Fatalf("turn status = %q, want completed", result.status)
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool iteration + final)", provider.callCount())
	}

	firstRequest := provider.callMessages(0)
	secondRequest := provider.callMessages(1)
	if got := loopFixContextMessagesOfKind(firstRequest, protocoltypes.ContextKindContextPressure); len(got) != 0 {
		t.Fatalf("iteration 1 unexpectedly injected %d context-pressure notices; the fixture must stay below the first band until iteration 2", len(got))
	}

	// 迭代 2 注入的尾巴从工作区增量（Baseline=false）开始。
	tailStart := loopFixFindLastIndex(secondRequest, func(msg providers.Message) bool {
		return msg.Context != nil &&
			msg.Context.Kind == protocoltypes.ContextKindWorkspaceInstructions &&
			!msg.Context.Baseline
	})
	if tailStart < 0 {
		t.Fatalf("iteration 2 request has no workspace-instruction delta; pre-step order 100 did not run:\n%s",
			loopFixDumpContexts(secondRequest))
	}

	wantTailKinds := []string{
		protocoltypes.ContextKindWorkspaceInstructions, // 100
		protocoltypes.ContextKindRuntimeContext,        // 200
		protocoltypes.ContextKindTaskState,             // 300
		protocoltypes.ContextKindContextPressure,       // 350
		"test-order-400",                               // 400
		protocoltypes.ContextKindLoopGuard,             // 500
	}
	gotTailKinds := loopFixKindsAfter(secondRequest, tailStart)
	if len(gotTailKinds) != len(wantTailKinds) {
		t.Fatalf("iteration 2 injected context kinds = %v, want %v (tailStart=%d)\n%s",
			gotTailKinds, wantTailKinds, tailStart, loopFixDumpContexts(secondRequest))
	}
	for i := range wantTailKinds {
		if gotTailKinds[i] != wantTailKinds[i] {
			t.Fatalf("iteration 2 context order = %v, want %v (pre-step table order)\n%s",
				gotTailKinds, wantTailKinds, loopFixDumpContexts(secondRequest))
		}
	}

	// 状态型上下文不得重复：supersede 在迭代 2 之后收敛了回合起点与回合内副本。
	for _, kind := range []string{
		protocoltypes.ContextKindRuntimeContext,
		protocoltypes.ContextKindTaskState,
		protocoltypes.ContextKindContextPressure,
		"test-order-400",
		protocoltypes.ContextKindLoopGuard,
	} {
		if got := loopFixContextMessagesOfKind(secondRequest, kind); len(got) != 1 {
			t.Fatalf("iteration 2 request carries %d %q messages, want exactly 1:\n%s",
				len(got), kind, loopFixDumpContexts(secondRequest))
		}
	}
	// 工作区基线 + 增量各一条（基线刻意保留）。
	if got := loopFixContextMessagesOfKind(secondRequest, protocoltypes.ContextKindWorkspaceInstructions); len(got) != 2 {
		t.Fatalf("workspace context messages = %d, want baseline+delta=2", len(got))
	}

	// 最新的任务状态（回合内第 3 次更新）必须可见，且旧目标不应再出现在请求里。
	taskStates := loopFixContextMessagesOfKind(secondRequest, protocoltypes.ContextKindTaskState)
	if !strings.Contains(taskStates[0].Content, "有序表回合任务 v3") {
		t.Fatalf("task-state content does not carry the newest objective:\n%s", taskStates[0].Content)
	}
	if !strings.Contains(taskStates[0].Content, "<task_state>") {
		t.Fatalf("task-state content is not the rendered block:\n%s", taskStates[0].Content)
	}
}

func loopFixDumpContexts(messages []providers.Message) string {
	var b strings.Builder
	for i, msg := range messages {
		kind := "<none>"
		if msg.Context != nil {
			kind = msg.Context.Kind
			if msg.Context.Baseline {
				kind += "(baseline)"
			}
		}
		fmt.Fprintf(&b, "[%d] role=%s kind=%s content_len=%d\n", i, msg.Role, kind, len(msg.Content))
	}
	return b.String()
}

// =============================================================================
// D4：supersede 在回合内（迭代 > 1）也必须生效
// =============================================================================

// TestTurnPreStepSupersedesTaskStateWithinTurn 覆盖 D4（P2）。
//
// 根因：supersedeContextMessages 只在 pipeline_setup（回合起点）调用一次；
// 回合内 pre-step 注入的新 task-state 会与回合起点副本同时留在下一次请求中。
//
// 断言：回合内工具改写任务状态后，第 2 次 provider 请求里 task-state 恰好一条，
// 且只包含最新目标（旧副本被替换）。
func TestTurnPreStepSupersedesTaskStateWithinTurn(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{
			content:      "更新任务状态后再回答。",
			finishReason: "tool_calls",
			toolCalls: []providers.ToolCall{{
				ID:        "call-bump",
				Type:      "function",
				Name:      "loop_fix_bump_task",
				Arguments: map[string]any{},
			}},
		},
		{content: "已按最新任务状态完成。", finishReason: "stop"},
	}}
	f := newLoopFixFixture(t, provider)
	sessionKey := "loop-fix-supersede-task-state"
	f.seedTaskState(t, sessionKey, "回合起点旧目标")

	bump := &loopFixBumpTaskTool{
		store:          f.store,
		sessionKey:     sessionKey,
		objectiveLabel: "回合内新目标",
	}
	f.agent.Tools.Register(bump)

	f.runTurn(t, sessionKey, "更新任务状态")

	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.callCount())
	}
	firstRequest := provider.callMessages(0)
	secondRequest := provider.callMessages(1)

	if got := loopFixContextMessagesOfKind(firstRequest, protocoltypes.ContextKindTaskState); len(got) != 1 {
		t.Fatalf("iteration 1 task-state messages = %d, want 1 (setup baseline)", len(got))
	}

	taskStates := loopFixContextMessagesOfKind(secondRequest, protocoltypes.ContextKindTaskState)
	if len(taskStates) != 1 {
		t.Fatalf("iteration 2 request carries %d task-state messages, want exactly 1 after supersede (D4):\n%s",
			len(taskStates), loopFixDumpContexts(secondRequest))
	}
	if !strings.Contains(taskStates[0].Content, "回合内新目标 v1") {
		t.Fatalf("kept task-state is not the newest one:\n%s", taskStates[0].Content)
	}
	if strings.Contains(taskStates[0].Content, "回合起点旧目标") {
		t.Fatalf("kept task-state still carries the superseded objective:\n%s", taskStates[0].Content)
	}
}

// =============================================================================
// D5：context-pressure 在 iteration 1（单次 LLM 调用回合）也必须可触发
// =============================================================================

// TestTurnContextPressureNoticeOnFirstIteration 覆盖 D5（P1）。
//
// 根因：临近上限提醒只挂在 pre-step（iteration > 1），单次 LLM 调用回合永不触发。
// 修复：在 SetupTurn 初始消息构建/压缩之后评估一次压力（与迭代路径共用同一
// band-once-per-turn 去重）。
//
// 子测试：
//  1. 接近上限的初始上下文 + 单次 LLM 调用回合 → 首次请求出现 kind=context-pressure；
//  2. 工具迭代回合 → 同一 band 在 iteration 1 注入后，iteration 2 不重复注入。
func TestTurnContextPressureNoticeOnFirstIteration(t *testing.T) {
	// 40000 ASCII 字符 ≈ 16000 tokens；加上 system/基线后跨过 0.85 band，
	// 但仍低于 isOverContextBudget 的硬阈值（不会触发 proactive 压缩）。
	nearLimitUserMessage := strings.Repeat("a", 40000)

	t.Run("single LLM call turn", func(t *testing.T) {
		provider := &loopFixProvider{steps: []loopFixLLMStep{{
			content:      "已处理。",
			finishReason: "stop",
		}}}
		f := newLoopFixFixture(t, provider)

		f.runTurn(t, "loop-fix-pressure-first", nearLimitUserMessage)

		if provider.callCount() != 1 {
			t.Fatalf("provider calls = %d, want 1 (single LLM call turn)", provider.callCount())
		}
		request := provider.callMessages(0)
		notices := loopFixContextMessagesOfKind(request, protocoltypes.ContextKindContextPressure)
		if len(notices) != 1 {
			t.Fatalf("iteration 1 request carries %d context-pressure notices, want 1 (D5):\n%s",
				len(notices), loopFixDumpContexts(request))
		}
		notice := notices[0]
		if notice.Role != "user" {
			t.Fatalf("context-pressure role = %q, want user", notice.Role)
		}
		if !strings.Contains(notice.Content, "<context_pressure>") || !strings.Contains(notice.Content, "上下文使用约") {
			t.Fatalf("context-pressure content is not the rendered notice:\n%s", notice.Content)
		}
		if notice.Context.Digest != "0.85" && notice.Context.Digest != "0.95" {
			t.Fatalf("context-pressure digest = %q, want 0.85 or 0.95", notice.Context.Digest)
		}
	})

	t.Run("tool iteration turn does not repeat the band", func(t *testing.T) {
		provider := &loopFixProvider{steps: []loopFixLLMStep{
			{
				content:      "先调用一次工具。",
				finishReason: "tool_calls",
				toolCalls: []providers.ToolCall{{
					ID:        "call-noop",
					Type:      "function",
					Name:      "loop_fix_noop",
					Arguments: map[string]any{},
				}},
			},
			{content: "完成。", finishReason: "stop"},
		}}
		f := newLoopFixFixture(t, provider)
		f.agent.Tools.Register(&loopFixNoopTool{})

		f.runTurn(t, "loop-fix-pressure-repeat", nearLimitUserMessage)

		if provider.callCount() != 2 {
			t.Fatalf("provider calls = %d, want 2", provider.callCount())
		}
		firstNotices := loopFixContextMessagesOfKind(
			provider.callMessages(0), protocoltypes.ContextKindContextPressure,
		)
		if len(firstNotices) != 1 {
			t.Fatalf("iteration 1 notices = %d, want 1 (D5)", len(firstNotices))
		}
		secondNotices := loopFixContextMessagesOfKind(
			provider.callMessages(1), protocoltypes.ContextKindContextPressure,
		)
		if len(secondNotices) != 1 {
			t.Fatalf("iteration 2 notices = %d, want 1 (same band must not repeat within a turn):\n%s",
				len(secondNotices), loopFixDumpContexts(provider.callMessages(1)))
		}
		if secondNotices[0].Context.Digest != firstNotices[0].Context.Digest {
			t.Fatalf("iteration 2 notice digest = %q, want same band %q",
				secondNotices[0].Context.Digest, firstNotices[0].Context.Digest)
		}
	})
}
