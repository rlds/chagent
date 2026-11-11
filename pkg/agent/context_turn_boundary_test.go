// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/bus"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// userContextMsg 构造指定 context kind 的运行时注入用户上下文消息。
func userContextMsg(kind, content string) providers.Message {
	return providers.Message{
		Role:    "user",
		Content: content,
		Context: &protocoltypes.ContextMeta{Kind: kind},
	}
}

// newCompressionTestLoop 构建带显式上下文窗口和输出预留的 AgentLoop，
// 返回压缩与摘要路径使用的 legacy 上下文管理器。
func newCompressionTestLoop(
	t *testing.T,
	contextWindow, maxTokens int,
	provider providers.LLMProvider,
) (*AgentLoop, *AgentInstance, *legacyContextManager) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Agents.Defaults.ContextWindow = contextWindow
	cfg.Agents.Defaults.MaxTokens = maxTokens

	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}
	if agent.ContextWindow != contextWindow {
		t.Fatalf("agent.ContextWindow = %d, want %d", agent.ContextWindow, contextWindow)
	}
	return al, agent, &legacyContextManager{al: al}
}

func TestTurnBoundariesSkipContext(t *testing.T) {
	t.Run("isTurnBoundary", func(t *testing.T) {
		tests := []struct {
			name string
			msg  providers.Message
			want bool
		}{
			{name: "plain user message", msg: msgUser("hi"), want: true},
			{name: "assistant message", msg: msgAssistant("hi"), want: false},
			{name: "tool result", msg: msgTool("tc1", "result"), want: false},
			{name: "workspace instructions", msg: userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "x"), want: false},
			{name: "runtime context", msg: userContextMsg(protocoltypes.ContextKindRuntimeContext, "x"), want: false},
			{name: "task state", msg: userContextMsg(protocoltypes.ContextKindTaskState, "x"), want: false},
			{name: "skill catalog", msg: userContextMsg(protocoltypes.ContextKindSkillCatalog, "x"), want: false},
			{name: "loop guard", msg: userContextMsg(protocoltypes.ContextKindLoopGuard, "x"), want: false},
			{name: "task round", msg: userContextMsg(protocoltypes.ContextKindTaskRound, "x"), want: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := isTurnBoundary(tt.msg); got != tt.want {
					t.Errorf("isTurnBoundary(%+v) = %v, want %v", tt.msg, got, tt.want)
				}
			})
		}
	})

	t.Run("parseTurnBoundaries", func(t *testing.T) {
		history := []providers.Message{
			msgUser("q1"),      // 0 ← 边界
			msgAssistant("a1"), // 1
			userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "instructions"), // 2
			msgAssistant("a2"), // 3
			userContextMsg(protocoltypes.ContextKindRuntimeContext, "runtime snapshot"), // 4
			msgAssistant("a3"), // 5
			msgUser("q2"),      // 6 ← 边界
			msgAssistant("a4"), // 7
			userContextMsg(protocoltypes.ContextKindTaskRound, "autonomous round"), // 8 ← 边界
			msgAssistant("a5"), // 9
			userContextMsg(protocoltypes.ContextKindLoopGuard, "loop reminder"), // 10
			msgAssistant("a6"), // 11
		}

		want := []int{0, 6, 8}
		got := parseTurnBoundaries(history)
		if len(got) != len(want) {
			t.Fatalf("parseTurnBoundaries() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseTurnBoundaries()[%d] = %d, want %d", i, got[i], want[i])
			}
		}
	})

	t.Run("isSafeBoundary", func(t *testing.T) {
		history := []providers.Message{
			msgUser("q1"),      // 0
			msgAssistant("a1"), // 1
			userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "instructions"), // 2
			msgAssistant("a2"), // 3
			userContextMsg(protocoltypes.ContextKindTaskRound, "round"), // 4
			msgAssistant("a3"), // 5
		}
		tests := []struct {
			index int
			want  bool
		}{
			{index: 0, want: true},  // 越界 → 安全
			{index: 2, want: false}, // 注入的上下文不是轮次起点
			{index: 4, want: true},  // 任务轮次是轮次起点
			{index: 1, want: false}, // assistant
			{index: 6, want: true},  // 历史末尾
		}
		for _, tt := range tests {
			if got := isSafeBoundary(history, tt.index); got != tt.want {
				t.Errorf("isSafeBoundary(history, %d) = %v, want %v", tt.index, got, tt.want)
			}
		}
	})
}

func TestFindSafeBoundarySkipsContext(t *testing.T) {
	t.Run("target on context message snaps forward to next real user", func(t *testing.T) {
		history := []providers.Message{
			msgUser("q1"),      // 0 ← 边界
			msgAssistant("a1"), // 1
			userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "instructions"), // 2
			msgAssistant("a2"), // 3
			msgUser("q2"),      // 4 ← 边界
			msgAssistant("a3"), // 5
		}

		if got := findSafeBoundary(history, 2); got != 4 {
			t.Errorf("findSafeBoundary(history, 2) = %d, want 4 (not the context message at 2)", got)
		}
	})

	t.Run("target inside tool chain skips leading context and full chain", func(t *testing.T) {
		history := []providers.Message{
			userContextMsg(protocoltypes.ContextKindRuntimeContext, "runtime snapshot"), // 0
			msgAssistant("a0"),       // 1
			msgUser("q1"),            // 2 ← 边界
			msgAssistantTC("tc1"),    // 3
			msgTool("tc1", "result"), // 4
			msgAssistant("a1"),       // 5
			msgUser("q2"),            // 6 ← 边界
			msgAssistant("a2"),       // 7
		}

		if got := findSafeBoundary(history, 4); got != 2 {
			t.Errorf("findSafeBoundary(history, 4) = %d, want 2", got)
		}
	})

	t.Run("only context messages means no turn boundary", func(t *testing.T) {
		history := []providers.Message{
			userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "instructions"), // 0
			msgAssistant("a0"), // 1
			userContextMsg(protocoltypes.ContextKindLoopGuard, "reminder"), // 2
			msgAssistant("a1"), // 3
		}

		if got := findSafeBoundary(history, 1); got != 1 {
			t.Errorf("findSafeBoundary(history, 1) = %d, want 1 (target unchanged)", got)
		}
		// 区间内每条用户消息都是注入上下文，因此没有安全的下标。
		for i := 1; i < len(history); i++ {
			if isSafeBoundary(history, i) {
				t.Errorf("isSafeBoundary(history, %d) = true, want false", i)
			}
		}
	})
}

func TestForceCompressionKeepsRealUserTurn(t *testing.T) {
	_, agent, lcm := newCompressionTestLoop(t, 100_000, 1000, &simpleMockProvider{response: "x"})

	const sessionKey = "session-turn-boundary"
	history := []providers.Message{
		msgUser("第一轮问题"),      // 0 ← 边界
		msgAssistant("第一轮回答"), // 1
		userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "WORKSPACE-ONLY"), // 2
		msgAssistant("注入后的回答"),                                                           // 3
		msgUser("第二轮真实问题"),                                                               // 4 ← 边界（保留）
		msgAssistantTC("tc1"),                                                            // 5
		msgTool("tc1", "tool result"),                                                    // 6
		msgAssistant("第二轮回答"),                                                            // 7
	}
	agent.Sessions.SetHistory(sessionKey, history)

	result, ok := lcm.forceCompression(sessionKey)
	if !ok {
		t.Fatal("forceCompression() = false, want true")
	}
	if result.DroppedMessages != 4 {
		t.Errorf("DroppedMessages = %d, want 4 (first Turn + injected context)", result.DroppedMessages)
	}
	if result.RemainingMessages != 4 {
		t.Errorf("RemainingMessages = %d, want 4", result.RemainingMessages)
	}

	kept := agent.Sessions.GetHistory(sessionKey)
	if len(kept) != 4 {
		t.Fatalf("kept history len = %d, want 4", len(kept))
	}
	// 保留的对话必须从真实用户回合开始，
	// 绝不能从注入的上下文消息开始。
	if kept[0].Role != "user" || kept[0].IsContextMessage() {
		t.Fatalf("kept[0] = %+v, want a real user message", kept[0])
	}
	if kept[0].Content != "第二轮真实问题" {
		t.Errorf("kept[0].Content = %q, want 第二轮真实问题", kept[0].Content)
	}

	// 工具调用序列必须保持完整。
	if len(kept[1].ToolCalls) != 1 || kept[1].ToolCalls[0].ID != "tc1" {
		t.Fatalf("kept[1] tool calls = %+v, want tc1 intact", kept[1].ToolCalls)
	}
	if kept[2].Role != "tool" || kept[2].ToolCallID != "tc1" {
		t.Errorf("kept[2] = %+v, want matching tool result", kept[2])
	}

	summary := agent.Sessions.GetSummary(sessionKey)
	if !strings.Contains(summary, "【紧急压缩】") || !strings.Contains(summary, "4") {
		t.Errorf("summary = %q, want Chinese emergency compression note with dropped count", summary)
	}
	if strings.Contains(summary, "Emergency compression") {
		t.Errorf("summary = %q, must not contain the old English note", summary)
	}
}

func TestForceCompressionPrunesToolResultsFirst(t *testing.T) {
	// 12000 字符的工具输出会让请求超限；把它裁剪为 4096+标记+1024 后请求回到窗口内，
	// 因此不得丢弃任何回合。
	_, agent, lcm := newCompressionTestLoop(t, 4800, 100, &simpleMockProvider{response: "x"})

	const sessionKey = "session-prune-first"
	toolContent := strings.Repeat("a", 12_000)
	history := []providers.Message{
		msgUser("q1"),
		msgAssistantTC("tc1"),
		msgTool("tc1", toolContent),
		msgAssistant("done"),
		msgUser("q2"),
		msgAssistant("answer"),
	}
	agent.Sessions.SetHistory(sessionKey, history)

	if !isOverContextBudget(agent.ContextWindow, history, nil, agent.MaxTokens) {
		t.Fatal("test setup: unpruned history should exceed the context window")
	}

	result, ok := lcm.forceCompression(sessionKey)
	if !ok {
		t.Fatal("forceCompression() = false, want true")
	}
	if result.DroppedMessages != 0 {
		t.Errorf("DroppedMessages = %d, want 0 (pruning alone should be enough)", result.DroppedMessages)
	}
	if result.RemainingMessages != len(history) {
		t.Errorf("RemainingMessages = %d, want %d", result.RemainingMessages, len(history))
	}

	kept := agent.Sessions.GetHistory(sessionKey)
	if len(kept) != len(history) {
		t.Fatalf("kept history len = %d, want %d (no Turn dropped)", len(kept), len(history))
	}
	pruned := kept[2].Content
	if !strings.Contains(pruned, "工具输出中段已裁剪") {
		t.Fatalf("tool result was not pruned: %q", pruned[:min(len(pruned), 64)])
	}
	if !strings.Contains(pruned, "完整输出：") {
		t.Fatalf("pruned tool result must name the full spilled output: %q",
			pruned[:min(len(pruned), 64)])
	}
	// 标记中的定位符必须指向可读的 spill 文件，其中包含完整的原始工具输出
	// （裁剪后仍可恢复全文）。
	spillPath := spilledPathFromContent(t, pruned)
	info, statErr := os.Stat(spillPath)
	if statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("spill locator %q is not a readable file: err=%v", spillPath, statErr)
	}
	stored, readErr := os.ReadFile(spillPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%q) error = %v", spillPath, readErr)
	}
	if string(stored) != toolContent {
		t.Errorf("spilled content len = %d, want original tool output len %d",
			len(stored), len(toolContent))
	}
	// 裁剪仍必须缩小请求：保留头尾并加上定位符标记，
	// 因此裁剪后的长度下界是头+尾，上界是原始长度。
	prunedRunes := len([]rune(pruned))
	if prunedRunes < 4096+1024 {
		t.Errorf("pruned tool content rune len = %d, want >= %d (head+tail)", prunedRunes, 4096+1024)
	}
	if prunedRunes >= len([]rune(toolContent)) {
		t.Errorf("pruned tool content rune len = %d, want < original %d",
			prunedRunes, len([]rune(toolContent)))
	}
	if isOverContextBudget(agent.ContextWindow, kept, nil, agent.MaxTokens) {
		t.Fatal("pruned history should now fit the context window")
	}
	if agent.Sessions.GetSummary(sessionKey) != "" {
		t.Errorf("summary should stay untouched when only pruning happens, got %q",
			agent.Sessions.GetSummary(sessionKey))
	}

	// 手工裁剪出的同一历史必须与该分支依赖的估算一致。
	if lcm.isHistoryOverBudget(agent, kept, "") {
		t.Error("isHistoryOverBudget(pruned) = true, want false")
	}
}

func TestTrimHistoryKeepsToolPairs(t *testing.T) {
	history := []providers.Message{
		msgUser(strings.Repeat("x", 4000)), // 0 ← 边界（丢弃）
		msgAssistantTC("tc1"),              // 1
		msgTool("tc1", "r1"),               // 2
		msgAssistant("done1"),              // 3
		userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, strings.Repeat("c", 400)), // 4
		msgAssistant("ctx followup"),      // 5
		msgUser(strings.Repeat("y", 100)), // 6 ← 边界（保留）
		msgAssistantTC("tc2"),             // 7
		msgTool("tc2", "r2"),              // 8
		msgAssistant("done2"),             // 9
		msgUser(strings.Repeat("z", 100)), // 10
		msgAssistant("a4"),                // 11
	}

	build := func(history []providers.Message) []providers.Message {
		return append([]providers.Message(nil), history...)
	}

	trimmed, messages, fit := trimHistoryToFitContextWindow(history, build, 800, nil, 0)
	if !fit {
		t.Fatal("expected trimmed history to fit the context window")
	}
	if len(trimmed) != 6 {
		t.Fatalf("trimmed len = %d, want 6 (Turn 2 + Turn 3)", len(trimmed))
	}
	// 裁剪必须从真实用户回合开始，而不是第一个回合内搭车的注入上下文消息
	// （下标 4）。
	if trimmed[0].IsContextMessage() || trimmed[0].Content != strings.Repeat("y", 100) {
		t.Fatalf("trimmed[0] = %+v, want real user turn at index 6", trimmed[0])
	}
	// 保留的工具调用序列必须完整。
	if len(trimmed[1].ToolCalls) != 1 || trimmed[1].ToolCalls[0].ID != "tc2" {
		t.Fatalf("trimmed[1] tool calls = %+v, want tc2 intact", trimmed[1].ToolCalls)
	}
	if trimmed[2].Role != "tool" || trimmed[2].ToolCallID != "tc2" {
		t.Errorf("trimmed[2] = %+v, want matching tc2 tool result", trimmed[2])
	}
	if isOverContextBudget(800, messages, nil, 0) {
		t.Error("trimmed messages should be within budget")
	}
}

func TestSummarizeSkipsContextMessages(t *testing.T) {
	provider := &recordingProvider{}
	al, agent, lcm := newCompressionTestLoop(t, 100_000, 4096, provider)

	const sessionKey = "session-summarize-context"
	history := []providers.Message{
		msgUser("Q1 content"),
		msgAssistant("A1 content"),
		userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "CTX_ONLY_MARKER 工作区指令不应进入摘要"),
		msgAssistant("A2 content"),
		msgUser("Q2 real"),
		msgAssistant("A3 content"),
		msgUser("Q3"),
		msgAssistant("A4"),
	}
	agent.Sessions.SetHistory(sessionKey, history)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t, al, 16, runtimeevents.KindAgentSessionSummarize)
	defer closeRuntimeEvents()

	lcm.summarizeSession(agent, sessionKey)

	if len(provider.lastMessages) == 0 {
		t.Fatal("summarization did not call the provider")
	}
	prompt := provider.lastMessages[0].Content
	if strings.Contains(prompt, "CTX_ONLY_MARKER") {
		t.Errorf("summary prompt must not include injected context, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Q1 content") || !strings.Contains(prompt, "A2 content") {
		t.Errorf("summary prompt should include ordinary conversation turns, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "为以下对话片段生成简明摘要") || !strings.Contains(prompt, "对话内容：") {
		t.Errorf("summary prompt should use the Chinese prompt, got:\n%s", prompt)
	}

	// 只保留最新的四条消息；上下文消息所属的回合继续保留。
	if kept := agent.Sessions.GetHistory(sessionKey); len(kept) != 4 {
		t.Fatalf("kept history len = %d, want 4", len(kept))
	}
	if summary := agent.Sessions.GetSummary(sessionKey); summary != "Mock response" {
		t.Errorf("summary = %q, want Mock response", summary)
	}

	events := collectRuntimeEventStream(runtimeCh)
	evt, ok := findRuntimeEvent(events, runtimeevents.KindAgentSessionSummarize)
	if !ok {
		t.Fatal("expected session summarize event")
	}
	payload, ok := evt.Payload.(SessionSummarizePayload)
	if !ok {
		t.Fatalf("expected SessionSummarizePayload, got %T", evt.Payload)
	}
	if payload.SummarizedMessages != 3 {
		t.Errorf("SummarizedMessages = %d, want 3 (context message excluded)", payload.SummarizedMessages)
	}
}

func TestSummarizePromptIsChinese(t *testing.T) {
	provider := &recordingProvider{}
	_, agent, lcm := newCompressionTestLoop(t, 100_000, 4096, provider)

	const sessionKey = "session-summarize-merge"
	history := make([]providers.Message, 0, 16)
	for i := 0; i < 8; i++ {
		history = append(history,
			msgUser("Q"+string(rune('a'+i))),
			msgAssistant("A"+string(rune('a'+i))),
		)
	}
	agent.Sessions.SetHistory(sessionKey, history)

	lcm.summarizeSession(agent, sessionKey)

	if len(provider.lastMessages) == 0 {
		t.Fatal("summarization did not call the provider")
	}
	mergePrompt := provider.lastMessages[0].Content
	if !strings.Contains(mergePrompt, "把以下两段对话摘要合并为一段连贯摘要：") {
		t.Errorf("merge prompt should be Chinese, got:\n%s", mergePrompt)
	}
	if strings.Contains(mergePrompt, "Merge these two conversation summaries") {
		t.Errorf("merge prompt must not contain the old English wording, got:\n%s", mergePrompt)
	}
	if strings.Contains(mergePrompt, "Provide a concise summary") {
		t.Errorf("batch prompt must not contain the old English wording, got:\n%s", mergePrompt)
	}
}

func TestSummarizeOmittedNoteIsChinese(t *testing.T) {
	provider := &recordingProvider{}
	_, agent, lcm := newCompressionTestLoop(t, 10_000, 4096, provider)

	const sessionKey = "session-summarize-omitted"
	// ContextWindow/2 = 5000 token；超过 10000 字符的内容会从摘要中省略。
	history := []providers.Message{
		msgUser("Q1"),
		msgAssistant(strings.Repeat("o", 12_000)),
		msgUser("Q2"),
		msgAssistant("A2"),
		msgUser("Q3"),
		msgAssistant("A3"),
		msgUser("Q4"),
		msgAssistant("A4"),
	}
	agent.Sessions.SetHistory(sessionKey, history)

	lcm.summarizeSession(agent, sessionKey)

	summary := agent.Sessions.GetSummary(sessionKey)
	if summary == "" {
		t.Fatal("expected a summary")
	}
	if !strings.Contains(summary, "[说明：为提升效率，部分过长消息未纳入本摘要。]") {
		t.Errorf("omitted note should be Chinese, got %q", summary)
	}
	if strings.Contains(summary, "Some oversized messages were omitted") {
		t.Errorf("summary must not contain the old English omitted note, got %q", summary)
	}
}
