// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// TestPreStepContextSourcesOrder 锁定 pre-step 顺序表。该顺序很关键：
// 运行时快照必须先于任务状态，loop-guard 通知必须最后刷新，
// 使模型在下一次请求前立即读到它们。
func TestPreStepContextSourcesOrder(t *testing.T) {
	sources := builtinPreStepContextSources
	if len(sources) != 6 {
		t.Fatalf("expected 6 pre-step context sources, got %d", len(sources))
	}
	wantOrder := []int{
		preStepOrderWorkspaceContext,
		preStepOrderRuntimeContext,
		preStepOrderTaskState,
		preStepOrderContextPressure,
		preStepOrderSkillCatalog,
		preStepOrderLoopGuard,
	}
	wantID := []string{
		"context.workspace",
		"context.runtime",
		"context.task_state",
		"context.pressure_notice",
		"context.skill_catalog",
		"context.loop_guard",
	}
	for i, source := range sources {
		if source.Order != wantOrder[i] {
			t.Errorf("source %d order = %d, want %d", i, source.Order, wantOrder[i])
		}
		if source.ID != wantID[i] {
			t.Errorf("source %d id = %q, want %q", i, source.ID, wantID[i])
		}
		if source.Inject == nil {
			t.Errorf("source %q has nil Inject", source.ID)
		}
		if i > 0 && sources[i-1].Order >= source.Order {
			t.Errorf("sources are not strictly ordered at index %d", i)
		}
	}
}

func TestFlushStepContextMessagesAppendsWithoutHistory(t *testing.T) {
	ts := &turnState{opts: processOptions{NoHistory: true}}
	ts.enqueueStepContextMessage(providers.Message{
		Role:    "user",
		Content: "检测到重复的工具调用。",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindLoopGuard},
	})
	ts.enqueueStepContextMessage(providers.Message{
		Role:    "user",
		Content: "第二次提醒。",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindLoopGuard},
	})
	if got := ts.pendingStepContextCount(); got != 2 {
		t.Fatalf("pending count = %d, want 2", got)
	}

	messages := []providers.Message{{Role: "assistant", Content: "working"}}
	out := ts.flushStepContextMessages(context.Background(), nil, messages)
	if len(out) != 3 {
		t.Fatalf("flushed messages = %d, want 3", len(out))
	}
	if out[1].Content != "检测到重复的工具调用。" || out[2].Content != "第二次提醒。" {
		t.Fatalf("unexpected order: %+v", out[1:])
	}
	// 生产者未填元数据时会被补齐。
	guard := out[1]
	if guard.PromptSource != "guard:loop_guard" {
		t.Errorf("PromptSource = %q, want guard:loop_guard", guard.PromptSource)
	}
	if guard.PromptLayer != string(PromptLayerTurn) {
		t.Errorf("PromptLayer = %q, want %q", guard.PromptLayer, PromptLayerTurn)
	}
	if guard.PromptSlot != "guard" {
		t.Errorf("PromptSlot = %q, want guard", guard.PromptSlot)
	}
	if got := ts.pendingStepContextCount(); got != 0 {
		t.Errorf("pending count after flush = %d, want 0", got)
	}

	// 第二次刷新是空操作。
	if out2 := ts.flushStepContextMessages(context.Background(), nil, out); len(out2) != len(out) {
		t.Errorf("second flush changed message count: %d -> %d", len(out), len(out2))
	}
}

func TestFlushStepContextMessagesSkipsBlankContent(t *testing.T) {
	ts := &turnState{opts: processOptions{NoHistory: true}}
	ts.enqueueStepContextMessage(providers.Message{Role: "user", Content: "   \n\t"})
	ts.enqueueStepContextMessage(providers.Message{Role: "user", Content: "有效提醒"})
	out := ts.flushStepContextMessages(context.Background(), nil, nil)
	if len(out) != 1 {
		t.Fatalf("flush kept %d messages, want 1", len(out))
	}
	if out[0].Content != "有效提醒" {
		t.Errorf("unexpected content %q", out[0].Content)
	}
}

// TestRunPreStepContextSourcesNoHistoryNoop 验证 NoHistory 回合绝不运行持久上下文注入器
// （它们没有可刷新的会话历史）。
func TestRunPreStepContextSourcesNoHistoryNoop(t *testing.T) {
	ts := &turnState{opts: processOptions{NoHistory: true}}
	in := []providers.Message{{Role: "user", Content: "hi"}}
	out := ts.runPreStepContextSources(context.Background(), nil, in)
	if len(out) != len(in) || out[0].Content != "hi" {
		t.Fatalf("NoHistory pre-step changed messages: %+v", out)
	}
}

func TestSupersedeContextMessages(t *testing.T) {
	runtimeOld := providers.Message{
		Role:    "user",
		Content: "old runtime",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindRuntimeContext},
	}
	runtimeNew := providers.Message{
		Role:    "user",
		Content: "new runtime",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindRuntimeContext},
	}
	taskOld := providers.Message{
		Role:    "user",
		Content: "old task",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindTaskState},
	}
	taskNew := providers.Message{
		Role:    "user",
		Content: "new task",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindTaskState},
	}
	workspace := providers.Message{
		Role:    "user",
		Content: "AGENTS.md baseline",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindWorkspaceInstructions, Baseline: true},
	}
	loopGuard := providers.Message{
		Role:    "user",
		Content: "repeat reminder",
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindLoopGuard},
	}
	realUser := providers.Message{Role: "user", Content: "do it"}
	assistant := providers.Message{Role: "assistant", Content: "ok"}

	history := []providers.Message{runtimeOld, workspace, taskOld, realUser, assistant, loopGuard, runtimeNew, taskNew}
	got := supersedeContextMessages(history)

	want := []string{
		"AGENTS.md baseline",
		"do it",
		"ok",
		"repeat reminder",
		"new runtime",
		"new task",
	}
	if len(got) != len(want) {
		t.Fatalf("supersede kept %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i, msg := range got {
		if msg.Content != want[i] {
			t.Errorf("message %d content = %q, want %q", i, msg.Content, want[i])
		}
	}

	// 没有上下文消息时保持不变（不要求切片身份相同，但内容顺序必须保持）。
	plain := []providers.Message{realUser, assistant}
	if out := supersedeContextMessages(plain); len(out) != 2 || out[0].Content != "do it" {
		t.Errorf("plain history changed: %+v", out)
	}
	if out := supersedeContextMessages(nil); len(out) != 0 {
		t.Errorf("nil history returned %d messages", len(out))
	}
}

// TestSupersedeKeepsOnlyNewestRuntimeSnapshot 覆盖长会话下的 DSH 替换语义：
// 多次周期性快照不会让请求不断增长。
func TestSupersedeKeepsOnlyNewestRuntimeSnapshot(t *testing.T) {
	var history []providers.Message
	for i := 0; i < 5; i++ {
		history = append(history, providers.Message{
			Role:    "user",
			Content: "snapshot",
			Context: &protocoltypes.ContextMeta{
				Kind:   protocoltypes.ContextKindRuntimeContext,
				Digest: string(rune('a' + i)),
			},
		})
	}
	got := supersedeContextMessages(history)
	if len(got) != 1 {
		t.Fatalf("kept %d snapshots, want 1", len(got))
	}
	if got[0].Context.Digest != "e" {
		t.Errorf("kept digest %q, want newest %q", got[0].Context.Digest, "e")
	}
}

// TestLoopGuardReminderFlushPath 检验 turnState 集成：重复相同的工具调用会排队提示，
// pre-step 刷新把它们追加在工具结果之后、下一次 LLM 请求之前。
func TestLoopGuardReminderFlushPath(t *testing.T) {
	ts := &turnState{
		opts:      processOptions{NoHistory: true},
		loopGuard: newRepeatToolGuard(DefaultLoopGuardConfig()),
	}
	args := map[string]any{"path": "AGENTS.md"}
	for i := 0; i < 2; i++ {
		ts.observeToolCallForLoopGuard("read_file", args)
	}
	if got := ts.pendingStepContextCount(); got != 0 {
		t.Fatalf("reminder queued before threshold: %d", got)
	}
	ts.observeToolCallForLoopGuard("read_file", args) // 第三次调用 → 温和提示
	if got := ts.pendingStepContextCount(); got != 1 {
		t.Fatalf("pending reminders = %d, want 1", got)
	}
	messages := []providers.Message{
		{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{{ID: "a"}}},
		{Role: "tool", Content: "result", ToolCallID: "a"},
	}
	out := ts.flushStepContextMessages(context.Background(), nil, messages)
	if len(out) != 3 {
		t.Fatalf("messages after flush = %d, want 3", len(out))
	}
	if !out[2].IsLoopGuard() {
		t.Fatalf("flushed message is not loop-guard: %+v", out[2])
	}
	if out[2].Role != "user" {
		t.Errorf("guard reminder role = %q, want user", out[2].Role)
	}
	if !strings.Contains(out[2].Content, "重复") {
		t.Errorf("guard reminder is not Chinese: %q", out[2].Content)
	}

	// 第四次调用低于下一个阈值；第五次排队详细提示。
	ts.observeToolCallForLoopGuard("read_file", args)
	if got := ts.pendingStepContextCount(); got != 0 {
		t.Fatalf("reminder queued at non-threshold count: %d", got)
	}
	ts.observeToolCallForLoopGuard("read_file", args)
	if got := ts.pendingStepContextCount(); got != 1 {
		t.Fatalf("detailed reminder pending = %d, want 1", got)
	}
	out = ts.flushStepContextMessages(context.Background(), nil, out)
	if !strings.Contains(out[len(out)-1].Content, "read_file") {
		t.Errorf("detailed reminder does not name the tool: %q", out[len(out)-1].Content)
	}
}

// TestLoopGuardResetOnUserInterjection 验证中途注入真实 steering 消息时使用的链重置。
func TestLoopGuardResetOnUserInterjection(t *testing.T) {
	ts := &turnState{
		opts:      processOptions{NoHistory: true},
		loopGuard: newRepeatToolGuard(DefaultLoopGuardConfig()),
	}
	args := map[string]any{"path": "x"}
	ts.observeToolCallForLoopGuard("read_file", args)
	ts.observeToolCallForLoopGuard("read_file", args)
	ts.resetLoopGuardChain()
	ts.observeToolCallForLoopGuard("read_file", args)
	ts.observeToolCallForLoopGuard("read_file", args)
	if got := ts.pendingStepContextCount(); got != 0 {
		t.Fatalf("reminder queued after reset: %d", got)
	}
	ts.observeToolCallForLoopGuard("read_file", args)
	if got := ts.pendingStepContextCount(); got != 1 {
		t.Fatalf("reminder not queued after post-reset threshold: %d", got)
	}
}
