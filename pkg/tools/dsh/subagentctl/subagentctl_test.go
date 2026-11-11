package subagentctl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// fake manager
// ---------------------------------------------------------------------------

type fakeManager struct {
	mu                        sync.Mutex
	tasks                     map[string]tools.SubagentTask
	cancel                    chan string
	waitErr                   error
	cancelErrNotInterruptible bool
}

func newFakeManager(tasks ...tools.SubagentTask) *fakeManager {
	m := &fakeManager{tasks: map[string]tools.SubagentTask{}, cancel: make(chan string, 8)}
	for _, task := range tasks {
		m.tasks[task.ID] = task
	}
	return m
}

func (f *fakeManager) ListTaskCopies() []tools.SubagentTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tools.SubagentTask, 0, len(f.tasks))
	for _, task := range f.tasks {
		out = append(out, task)
	}
	return out
}

func (f *fakeManager) GetTaskCopy(taskID string) (tools.SubagentTask, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[taskID]
	return task, ok
}

func (f *fakeManager) CancelTask(taskID string) (tools.SubagentTask, error) {
	f.mu.Lock()
	task, ok := f.tasks[taskID]
	f.mu.Unlock()
	if !ok {
		return tools.SubagentTask{}, tools.ErrSubagentTaskNotFound
	}
	if task.Status != tools.SubagentStatusRunning {
		return task, tools.ErrSubagentTaskNotRunning
	}
	if f.cancelErrNotInterruptible {
		return task, tools.ErrSubagentTaskNotInterruptible
	}
	return task, nil
}

func (f *fakeManager) WaitTask(ctx context.Context, taskID string, poll time.Duration) (tools.SubagentTask, error) {
	if f.waitErr != nil {
		return tools.SubagentTask{}, f.waitErr
	}
	for {
		task, ok := f.GetTaskCopy(taskID)
		if !ok {
			return tools.SubagentTask{}, tools.ErrSubagentTaskNotFound
		}
		if task.Status != tools.SubagentStatusRunning {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func completedTask(id, channel, chatID string) tools.SubagentTask {
	return tools.SubagentTask{
		ID: id, Task: "do " + id, Label: "lbl-" + id, Status: tools.SubagentStatusCompleted,
		Result: "result for " + id, Created: 1700000000000,
		OriginChannel: channel, OriginChatID: chatID,
	}
}

func runningTask(id, channel, chatID string) tools.SubagentTask {
	return tools.SubagentTask{
		ID: id, Task: "do " + id, Status: tools.SubagentStatusRunning,
		Created: 1700000000001, OriginChannel: channel, OriginChatID: chatID,
	}
}

// ---------------------------------------------------------------------------
// metadata
// ---------------------------------------------------------------------------

func TestTools_MetadataAndParameters(t *testing.T) {
	mgr := newFakeManager()
	toolsToCheck := []struct {
		name        string
		tool        interface{ Name() string }
		description func() string
		params      func() map[string]any
	}{
		{"list_agents", NewListAgentsTool(mgr), NewListAgentsTool(mgr).Description, NewListAgentsTool(mgr).Parameters},
		{"wait_agent", NewWaitAgentTool(mgr), NewWaitAgentTool(mgr).Description, NewWaitAgentTool(mgr).Parameters},
		{"interrupt_agent", NewInterruptAgentTool(mgr), NewInterruptAgentTool(mgr).Description, NewInterruptAgentTool(mgr).Parameters},
	}
	for _, item := range toolsToCheck {
		if got := item.tool.Name(); got != item.name {
			t.Errorf("name = %q, want %q", got, item.name)
		}
		if desc := item.description(); !strings.Contains(desc, "subagent") {
			t.Errorf("%s description should mention subagent, got %q", item.name, desc)
		}
		params := item.params()
		if params["type"] != "object" {
			t.Errorf("%s parameters type = %v", item.name, params["type"])
		}
	}

	listMeta := NewListAgentsTool(mgr).PromptMetadata()
	waitMeta := NewWaitAgentTool(mgr).PromptMetadata()
	interruptMeta := NewInterruptAgentTool(mgr).PromptMetadata()
	for name, meta := range map[string]struct {
		layer, slot, source string
	}{
		"list_agents":     {listMeta.Layer, listMeta.Slot, listMeta.Source},
		"wait_agent":      {waitMeta.Layer, waitMeta.Slot, waitMeta.Source},
		"interrupt_agent": {interruptMeta.Layer, interruptMeta.Slot, interruptMeta.Source},
	} {
		if meta.layer != "capability" || meta.slot != "dsh" || meta.source != "tool_registry:dsh" {
			t.Errorf("%s PromptMetadata = %+v, want capability/dsh/tool_registry:dsh", name, meta)
		}
	}

	if !NewListAgentsTool(mgr).ConcurrencySafe() {
		t.Error("list_agents should be concurrency-safe")
	}
	if !NewWaitAgentTool(mgr).ConcurrencySafe() {
		t.Error("wait_agent should be concurrency-safe")
	}
	if NewInterruptAgentTool(mgr).ConcurrencySafe() {
		t.Error("interrupt_agent must not be concurrency-safe")
	}
}

// TestRegister verifies the three tools land in the registry.
func TestRegister(t *testing.T) {
	mgr := newFakeManager()
	registry := tools.NewToolRegistry()
	Register(registry, nil)
	for _, name := range []string{ListAgentsToolName, WaitAgentToolName, InterruptAgentToolName} {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("tool %q was not registered", name)
		}
	}
	_ = mgr
}

// ---------------------------------------------------------------------------
// list_agents
// ---------------------------------------------------------------------------

func TestListAgents_Empty(t *testing.T) {
	tool := NewListAgentsTool(newFakeManager())
	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "没有可用的 subagent 任务") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestListAgents_NilManager(t *testing.T) {
	result := NewListAgentsTool(nil).Execute(context.Background(), map[string]any{})
	if !result.IsError || !strings.Contains(result.ForLLM, "未配置") {
		t.Errorf("want not-configured error, got error=%v msg=%s", result.IsError, result.ForLLM)
	}
	if !errors.Is(result.Err, ErrManagerNotConfigured) {
		t.Errorf("Err = %v, want ErrManagerNotConfigured", result.Err)
	}
}

func TestListAgents_VisibilityAndStatusFilter(t *testing.T) {
	mgr := newFakeManager(
		runningTask("subagent-1", "pico", "chat-a"),
		completedTask("subagent-2", "pico", "chat-a"),
		completedTask("subagent-3", "pico", "chat-b"),
		completedTask("subagent-4", "telegram", "chat-c"),
	)
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")

	result := NewListAgentsTool(mgr).Execute(ctx, map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "subagent-1") || !strings.Contains(result.ForLLM, "subagent-2") {
		t.Errorf("expected own-chat tasks, got: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "subagent-3") || strings.Contains(result.ForLLM, "subagent-4") {
		t.Errorf("leaked other-chat tasks: %s", result.ForLLM)
	}

	filtered := NewListAgentsTool(mgr).Execute(ctx, map[string]any{"status": "completed"})
	if !strings.Contains(filtered.ForLLM, "subagent-2") || strings.Contains(filtered.ForLLM, "subagent-1") {
		t.Errorf("status filter failed: %s", filtered.ForLLM)
	}

	bad := NewListAgentsTool(mgr).Execute(ctx, map[string]any{"status": "bogus"})
	if !bad.IsError || !strings.Contains(bad.ForLLM, "status") {
		t.Errorf("invalid status should be a diagnosable error: error=%v msg=%s", bad.IsError, bad.ForLLM)
	}
}

func TestListAgents_LimitClampAndTruncationFlag(t *testing.T) {
	mgr := newFakeManager()
	for i := 1; i <= 5; i++ {
		mgr.tasks["subagent-"+string(rune('0'+i))] = completedTask(
			"subagent-"+string(rune('0'+i)), "pico", "chat-a")
	}
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewListAgentsTool(mgr).Execute(ctx, map[string]any{"limit": 2})
	if !strings.Contains(result.ForLLM, "共 5 条") || !strings.Contains(result.ForLLM, "已截断") {
		t.Errorf("expected truncation note, got: %s", result.ForLLM)
	}
}

func TestListAgents_ResultTruncated(t *testing.T) {
	long := strings.Repeat("很长的结果", 400)
	task := completedTask("subagent-1", "pico", "chat-a")
	task.Result = long
	mgr := newFakeManager(task)
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewListAgentsTool(mgr).Execute(ctx, map[string]any{})
	if len([]rune(result.ForLLM)) > 2000 {
		t.Errorf("result not truncated, length=%d", len([]rune(result.ForLLM)))
	}
}

// ---------------------------------------------------------------------------
// wait_agent
// ---------------------------------------------------------------------------

func TestWaitAgent_ImmediatelyCompleted(t *testing.T) {
	mgr := newFakeManager(completedTask("subagent-1", "pico", "chat-a"))
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewWaitAgentTool(mgr).Execute(ctx, map[string]any{"task_id": "subagent-1"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "status=completed") || !strings.Contains(result.ForLLM, "result for subagent-1") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestWaitAgent_TimeoutReportsRunning(t *testing.T) {
	mgr := newFakeManager(runningTask("subagent-1", "pico", "chat-a"))
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	start := time.Now()
	result := NewWaitAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": "subagent-1", "timeout_ms": 150,
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait timeout not honored: %s", elapsed)
	}
	if !result.IsError {
		t.Fatalf("timeout must be an error result, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "超时") || !strings.Contains(result.ForLLM, "status=running") {
		t.Errorf("timeout message must report truthfully: %s", result.ForLLM)
	}
}

func TestWaitAgent_NotFoundAndInvisible(t *testing.T) {
	mgr := newFakeManager(completedTask("subagent-1", "pico", "chat-a"))
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-b")
	result := NewWaitAgentTool(mgr).Execute(ctx, map[string]any{"task_id": "subagent-1"})
	if !result.IsError || !strings.Contains(result.ForLLM, "找不到") {
		t.Errorf("invisible task should look not-found: error=%v msg=%s", result.IsError, result.ForLLM)
	}
	missing := NewWaitAgentTool(mgr).Execute(context.Background(), map[string]any{"task_id": "nope"})
	if !missing.IsError {
		t.Errorf("missing task should error")
	}
	if bad := NewWaitAgentTool(mgr).Execute(context.Background(), map[string]any{}); !bad.IsError ||
		!strings.Contains(bad.ForLLM, "task_id") {
		t.Errorf("missing task_id should be diagnosable: %s", bad.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// interrupt_agent
// ---------------------------------------------------------------------------

func TestInterruptAgent_TaskAlreadyFinished(t *testing.T) {
	mgr := newFakeManager(completedTask("subagent-1", "pico", "chat-a"))
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{"task_id": "subagent-1"})
	if result.IsError {
		t.Fatalf("finished task should not be an error, got %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "无需中断") || !strings.Contains(result.ForLLM, "completed") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestInterruptAgent_NotInterruptibleIsExplicitError(t *testing.T) {
	mgr := newFakeManager(runningTask("subagent-1", "pico", "chat-a"))
	mgr.cancelErrNotInterruptible = true
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{"task_id": "subagent-1"})
	if !result.IsError {
		t.Fatalf("not-interruptible must be an error result, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "不支持中断当前任务") {
		t.Errorf("error must state the real limitation: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "wait_agent") {
		t.Errorf("error should suggest wait_agent: %s", result.ForLLM)
	}
}

func TestInterruptAgent_NotFound(t *testing.T) {
	mgr := newFakeManager()
	result := NewInterruptAgentTool(mgr).Execute(context.Background(), map[string]any{"task_id": "nope"})
	if !result.IsError || !strings.Contains(result.ForLLM, "找不到") {
		t.Errorf("want not-found error, got error=%v msg=%s", result.IsError, result.ForLLM)
	}
}

func TestInterruptAgent_SuccessfullySignalsCancellation(t *testing.T) {
	mgr := newFakeManager(runningTask("subagent-1", "pico", "chat-a"))
	// Simulate an executor that honors ctx cancellation and reports canceled.
	go func() {
		time.Sleep(60 * time.Millisecond)
		mgr.mu.Lock()
		task := mgr.tasks["subagent-1"]
		task.Status = tools.SubagentStatusCanceled
		task.Result = "canceled by test"
		mgr.tasks["subagent-1"] = task
		mgr.mu.Unlock()
	}()
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": "subagent-1", "wait_ms": 1500,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "已中断") || !strings.Contains(result.ForLLM, "canceled") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestInterruptAgent_SignaledButStillRunningIsHonest(t *testing.T) {
	mgr := newFakeManager(runningTask("subagent-1", "pico", "chat-a"))
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": "subagent-1", "wait_ms": 120,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "仍为 status=running") || !strings.Contains(result.ForLLM, "不代表已中断") {
		t.Errorf("must not claim cancellation succeeded: %s", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// real SubagentManager integration
// ---------------------------------------------------------------------------

func TestRealManager_TrackWaitInterrupt(t *testing.T) {
	mgr := tools.NewSubagentManager(nil, "test-model", t.TempDir())

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	handle, err := mgr.TrackSpawn(parent, "long task", "label", "", "pico", "chat-a")
	if err != nil {
		t.Fatalf("TrackSpawn: %v", err)
	}
	finished := make(chan struct{})
	go func() {
		<-handle.Ctx.Done()
		handle.Finish(tools.SubagentStatusCanceled, "canceled by interrupt")
		close(finished)
	}()

	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")

	// wait_agent times out while the task is still running (truthful report).
	waitResult := NewWaitAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": handle.ID, "timeout_ms": 120,
	})
	if !waitResult.IsError || !strings.Contains(waitResult.ForLLM, "status=running") {
		t.Fatalf("wait should time out truthfully, got error=%v msg=%s", waitResult.IsError, waitResult.ForLLM)
	}

	// interrupt_agent cancels the context and observes the terminal state.
	interruptResult := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": handle.ID, "wait_ms": 2000,
	})
	if interruptResult.IsError {
		t.Fatalf("interrupt failed: %s", interruptResult.ForLLM)
	}
	if !strings.Contains(interruptResult.ForLLM, "canceled") {
		t.Errorf("interrupt should report canceled: %s", interruptResult.ForLLM)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("task goroutine did not observe cancellation")
	}

	task, ok := mgr.GetTaskCopy(handle.ID)
	if !ok || task.Status != tools.SubagentStatusCanceled {
		t.Errorf("manager task status = %q, want canceled", task.Status)
	}

	// list_agents reflects the canceled task for the same chat and hides it from others.
	list := NewListAgentsTool(mgr).Execute(ctx, map[string]any{})
	if list.IsError || !strings.Contains(list.ForLLM, "canceled") {
		t.Errorf("list should show canceled task, got error=%v msg=%s", list.IsError, list.ForLLM)
	}
	otherCtx := tools.WithToolContext(context.Background(), "pico", "chat-b")
	hidden := NewListAgentsTool(mgr).Execute(otherCtx, map[string]any{})
	if !hidden.IsError && strings.Contains(hidden.ForLLM, handle.ID) {
		t.Errorf("task leaked to another chat: %s", hidden.ForLLM)
	}
}

func TestRealManager_FinishCompletedVisibleToWait(t *testing.T) {
	mgr := tools.NewSubagentManager(nil, "test-model", t.TempDir())
	handle, err := mgr.TrackSpawn(context.Background(), "quick", "", "", "pico", "chat-a")
	if err != nil {
		t.Fatalf("TrackSpawn: %v", err)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		handle.Finish(tools.SubagentStatusCompleted, "done fine")
	}()
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewWaitAgentTool(mgr).Execute(ctx, map[string]any{
		"task_id": handle.ID, "timeout_ms": 3000,
	})
	if result.IsError {
		t.Fatalf("wait failed: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "completed") || !strings.Contains(result.ForLLM, "done fine") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestRealManager_CancelCompletedTaskReportsNotRunning(t *testing.T) {
	mgr := tools.NewSubagentManager(nil, "test-model", t.TempDir())
	handle, err := mgr.TrackSpawn(context.Background(), "quick", "", "", "pico", "chat-a")
	if err != nil {
		t.Fatalf("TrackSpawn: %v", err)
	}
	handle.Finish(tools.SubagentStatusCompleted, "done")
	ctx := tools.WithToolContext(context.Background(), "pico", "chat-a")
	result := NewInterruptAgentTool(mgr).Execute(ctx, map[string]any{"task_id": handle.ID})
	if result.IsError {
		t.Fatalf("completed task interrupt should be informational, got %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "无需中断") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}

func TestRealManager_InterruptUnregisteredTaskIsNotFound(t *testing.T) {
	mgr := tools.NewSubagentManager(nil, "test-model", t.TempDir())
	result := NewInterruptAgentTool(mgr).Execute(context.Background(), map[string]any{"task_id": "subagent-404"})
	if !result.IsError {
		t.Fatalf("want error for unknown task")
	}
	if !strings.Contains(result.ForLLM, "找不到") {
		t.Errorf("unexpected message: %s", result.ForLLM)
	}
}
