// Chagent - 超轻量级个人 AI agent
//
// R8 收尾：loop-guard 三处接线中两处（hook respond 分支、并行批调度路径）
// 的专项测试。两者都通过真实 runAgentLoop → ExecuteTools 路径驱动，
// 断言重复工具守卫链是否按设计记录这些调用：
//   - hook respond 分支：hook 直接给出工具结果（不执行真实工具），但该调用
//     已到达执行阶段，因此必须计入重复工具守卫链；被 deny 的调用则不计数；
//   - 并行批调度路径：同一模型步内的并行批工具调用同样必须在派发前的
//     计划阶段计数，使第 3 次相同调用触发阈值并在下一次请求注入 loop-guard。

package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/tools"
)

// loopGuardCountingTool 是记录真实执行次数的探针工具。
// hook respond 测试用它证明“工具没有被真正执行”；
// 批调度测试用它证明“所有批内调用都被真正执行”。
type loopGuardCountingTool struct {
	name     string
	executed atomic.Int64
}

func (t *loopGuardCountingTool) Name() string { return t.name }

func (t *loopGuardCountingTool) Description() string {
	return "counting probe for loop-guard wiring tests"
}

func (t *loopGuardCountingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *loopGuardCountingTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	t.executed.Add(1)
	return tools.SilentResult("executed")
}

// repeatedToolCalls 构造 n 个同 id 前缀、同工具、同参数的调用，
// 以命中重复工具守卫的链式计数（守卫链键 = 工具名 + 规范化参数 JSON）。
func repeatedToolCalls(idPrefix, toolName string, args map[string]any, n int) []providers.ToolCall {
	calls := make([]providers.ToolCall, 0, n)
	for i := 0; i < n; i++ {
		calls = append(calls, providers.ToolCall{
			ID:        idPrefix + "-" + string(rune('a'+i)),
			Type:      "function",
			Name:      toolName,
			Arguments: args,
		})
	}
	return calls
}

// loopGuardMessageCount 返回请求消息中 loop-guard 上下文消息的条数。
func loopGuardMessageCount(messages []providers.Message) int {
	return len(loopFixContextMessagesOfKind(messages, protocoltypes.ContextKindLoopGuard))
}

// toolMessagesContaining 返回内容包含 needle 的 tool 角色消息条数。
func toolMessagesContaining(messages []providers.Message, needle string) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, needle) {
			count++
		}
	}
	return count
}

// TestExecuteToolsHookRespondLoopGuardObservation 覆盖 loop-guard 的
// pipeline_execute.go hook-respond 接线（observeToolCallForLoopGuard 在
// HookActionRespond 分支被调用）。
//
// 语义（按生产代码注释）：hook 直接提供结果的调用已经到达执行阶段，
// 因此计入重复链；被 HookActionDenyTool 拒绝的调用在 observe 之前 continue，
// 因此刻意不计数。若任一处接线回退，下一请求中的 loop-guard 上下文会缺失/误现。
func TestExecuteToolsHookRespondLoopGuardObservation(t *testing.T) {
	t.Run("hook respond calls count toward the guard chain", func(t *testing.T) {
		provider := &loopFixProvider{steps: []loopFixLLMStep{
			{
				content:      "调用三次同名工具。",
				finishReason: "tool_calls",
				toolCalls: repeatedToolCalls(
					"respond",
					"echo_text",
					map[string]any{"text": "same"},
					3,
				),
			},
			{content: "done", finishReason: "stop"},
		}}
		al, agent, cleanup := newHookTestLoop(t, provider)
		defer cleanup()

		probe := &loopGuardCountingTool{name: "echo_text"}
		al.RegisterTool(probe)
		if err := al.MountHook(NamedHook("respond-hook", &respondHook{
			respondTools: map[string]bool{"echo_text": true},
		})); err != nil {
			t.Fatalf("MountHook failed: %v", err)
		}

		resp, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
			SessionKey:      "loop-guard-hook-respond",
			Channel:         "cli",
			ChatID:          "direct",
			UserMessage:     "重复调用 echo_text",
			DefaultResponse: defaultResponse,
			EnableSummary:   false,
			SendResponse:    false,
		})
		if err != nil {
			t.Fatalf("runAgentLoop failed: %v", err)
		}
		if resp != "done" {
			t.Fatalf("response = %q, want %q", resp, "done")
		}
		// hook respond 分支必须跳过真实工具执行。
		if got := probe.executed.Load(); got != 0 {
			t.Fatalf("probe executed %d times, want 0 (HookActionRespond must skip tool execution)", got)
		}
		if got := provider.callCount(); got != 2 {
			t.Fatalf("provider calls = %d, want 2 (tool iteration + final)", got)
		}

		first := provider.callMessages(0)
		second := provider.callMessages(1)
		if got := len(first); got == 0 {
			t.Fatal("first request is empty")
		}
		// 三次 hook respond 结果都必须出现在下一次请求里（每种一条）。
		if got := toolMessagesContaining(second, "hook-responded: echo_text"); got != 3 {
			t.Fatalf("hook-respond tool results = %d, want 3:\n%s", got, loopFixDumpContexts(second))
		}
		// 第三次 hook respond 调用必须命中阈值 3，并在下一请求注入一条 loop-guard。
		if got := loopGuardMessageCount(second); got != 1 {
			t.Fatalf("hook-respond path queued %d loop-guard messages, want 1 (call must count toward the chain):\n%s",
				got, loopFixDumpContexts(second))
		}
		guard := loopFixContextMessagesOfKind(second, protocoltypes.ContextKindLoopGuard)[0]
		if guard.Role != "user" {
			t.Fatalf("loop-guard role = %q, want user", guard.Role)
		}
		if !strings.Contains(guard.Content, "重复") {
			t.Fatalf("loop-guard reminder is not the Chinese repeat notice:\n%s", guard.Content)
		}
	})

	t.Run("denied calls do not count toward the guard chain", func(t *testing.T) {
		provider := &loopFixProvider{steps: []loopFixLLMStep{
			{
				content:      "两次 respond、一次 deny。",
				finishReason: "tool_calls",
				toolCalls: append(
					repeatedToolCalls("respond", "echo_text", map[string]any{"text": "same"}, 2),
					providers.ToolCall{
						ID:        "deny-a",
						Type:      "function",
						Name:      "blocked_tool",
						Arguments: map[string]any{"path": "x"},
					},
				),
			},
			{content: "done", finishReason: "stop"},
		}}
		al, agent, cleanup := newHookTestLoop(t, provider)
		defer cleanup()

		probe := &loopGuardCountingTool{name: "echo_text"}
		al.RegisterTool(probe)
		if err := al.MountHook(NamedHook("respond-hook", &respondHook{
			respondTools: map[string]bool{"echo_text": true},
		})); err != nil {
			t.Fatalf("MountHook failed: %v", err)
		}
		if err := al.MountHook(NamedHook("deny-hook", &denyToolHook{
			denyTools: map[string]bool{"blocked_tool": true},
		})); err != nil {
			t.Fatalf("MountHook failed: %v", err)
		}

		if _, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
			SessionKey:      "loop-guard-hook-deny",
			Channel:         "cli",
			ChatID:          "direct",
			UserMessage:     "两次 respond 一次 deny",
			DefaultResponse: defaultResponse,
			EnableSummary:   false,
			SendResponse:    false,
		}); err != nil {
			t.Fatalf("runAgentLoop failed: %v", err)
		}

		if got := probe.executed.Load(); got != 0 {
			t.Fatalf("probe executed %d times, want 0 (respond+deny must skip execution)", got)
		}
		second := provider.callMessages(1)
		if got := toolMessagesContaining(second, "hook-responded: echo_text"); got != 2 {
			t.Fatalf("hook-respond tool results = %d, want 2", got)
		}
		if got := toolMessagesContaining(second, "denied by hook"); got != 1 {
			t.Fatalf("denied tool results = %d, want 1:\n%s", got, loopFixDumpContexts(second))
		}
		// 只有 2 次已到达执行阶段的调用计数，低于阈值 3：不得出现 loop-guard。
		if got := loopGuardMessageCount(second); got != 0 {
			t.Fatalf("denied calls leaked into the guard chain: %d loop-guard messages, want 0:\n%s",
				got, loopFixDumpContexts(second))
		}
	})
}

// TestExecuteToolsBatchLoopGuardObservation 覆盖 loop-guard 的
// pipeline_execute.go 并行批调度接线（observeToolCallForLoopGuard 在批次
// 计划阶段对每个未被拒绝的调用执行）。
//
// 模型第 1 次响应给出 3 个同工具同参数的并行安全调用：批次路径必须
// 全部并发执行（maxSeen>=2），并且 3 次调用都计入守卫链 → 第 2 次请求
// 恰好出现 1 条 loop-guard 上下文。若批调度路径漏掉 observe，3 次调用
// 不会命中阈值 3，断言失败。
func TestExecuteToolsBatchLoopGuardObservation(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{
			content:      "并行读取三次同一路径。",
			finishReason: "tool_calls",
			toolCalls: repeatedToolCalls(
				"batch",
				"read_file",
				map[string]any{"path": "same.txt"},
				3,
			),
		},
		{content: "done", finishReason: "stop"},
	}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	probe := newBatchProbeTool()
	al.RegisterTool(probe)

	if _, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "loop-guard-batch",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "并行重复读取同一文件",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	}); err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}

	// 批内三次调用都必须真正执行。
	if got := probe.executed.Load(); got != 3 {
		t.Fatalf("probe executed %d calls, want 3", got)
	}
	// maxSeen>=2 证明走的是并行批调度路径，而不是串行回退。
	if got := probe.maxSeen.Load(); got < 2 {
		t.Fatalf("read_file probe max concurrent = %d, want >= 2 (batch path not used)", got)
	}
	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (batch iteration + final)", got)
	}

	second := provider.callMessages(1)
	var toolResultIDs []string
	for _, msg := range second {
		if msg.Role == "tool" {
			toolResultIDs = append(toolResultIDs, msg.ToolCallID)
		}
	}
	if len(toolResultIDs) != 3 {
		t.Fatalf("tool results = %v, want 3 results in model order", toolResultIDs)
	}
	for i, want := range repeatedToolCalls("batch", "read_file", nil, 3) {
		if toolResultIDs[i] != want.ID {
			t.Fatalf("tool result order = %v, want model order ending with %q", toolResultIDs, want.ID)
		}
	}

	if got := loopGuardMessageCount(second); got != 1 {
		t.Fatalf("batch path queued %d loop-guard messages, want 1 (batch calls must count toward the chain):\n%s",
			got, loopFixDumpContexts(second))
	}
	guard := loopFixContextMessagesOfKind(second, protocoltypes.ContextKindLoopGuard)[0]
	if !strings.Contains(guard.Content, "重复") {
		t.Fatalf("loop-guard reminder is not the Chinese repeat notice:\n%s", guard.Content)
	}
}
