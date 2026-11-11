package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingSpawner blocks until its context is canceled, then reports the error.
type blockingSpawner struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingSpawner) SpawnSubTurn(ctx context.Context, _ SubTurnConfig) (*ToolResult, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSpawnTool_TracksTaskAndIsInterruptible verifies that the spawn tool
// registers its background task with the shared SubagentManager so the DSH
// control plane can list/wait/interrupt it, and that interrupt actually
// cancels the execution context.
func TestSpawnTool_TracksTaskAndIsInterruptible(t *testing.T) {
	mgr := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())
	spawnTool := NewSpawnTool(mgr)
	spawner := &blockingSpawner{started: make(chan struct{})}
	spawnTool.SetSpawner(spawner)

	ctx := WithToolContext(context.Background(), "pico", "chat-a")
	result := spawnTool.Execute(ctx, map[string]any{"task": "long job", "label": "long"})
	if result == nil || result.IsError {
		t.Fatalf("spawn should be accepted, got %+v", result)
	}

	select {
	case <-spawner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("spawn goroutine did not start")
	}

	// Locate the tracked task (subagent-1 for a fresh manager).
	task, ok := mgr.GetTaskCopy("subagent-1")
	if !ok {
		t.Fatalf("tracked task not registered")
	}
	if task.Status != SubagentStatusRunning {
		t.Fatalf("task status = %q, want running", task.Status)
	}
	if task.OriginChannel != "pico" || task.OriginChatID != "chat-a" {
		t.Errorf("task origin = %q/%q, want pico/chat-a", task.OriginChannel, task.OriginChatID)
	}

	if _, err := mgr.CancelTask("subagent-1"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ = mgr.GetTaskCopy("subagent-1")
		if task.Status != SubagentStatusRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task.Status != SubagentStatusCanceled {
		t.Fatalf("task status = %q, want canceled", task.Status)
	}
}

// TestSubagentManager_WaitTaskCompletes verifies WaitTask returns terminal
// state for both completed and canceled tasks.
func TestSubagentManager_WaitTaskCompletes(t *testing.T) {
	mgr := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())
	handle, err := mgr.TrackSpawn(context.Background(), "task", "", "", "pico", "chat-a")
	if err != nil {
		t.Fatalf("TrackSpawn: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		handle.Finish(SubagentStatusCompleted, "done")
	}()
	done, err := mgr.WaitTask(context.Background(), handle.ID, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if done.Status != SubagentStatusCompleted || done.Result != "done" {
		t.Errorf("WaitTask = %+v", done)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	handle2, _ := mgr.TrackSpawn(context.Background(), "stuck", "", "", "pico", "chat-a")
	if _, err := mgr.WaitTask(timeoutCtx, handle2.ID, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitTask timeout err = %v, want DeadlineExceeded", err)
	}
}

// TestSubagentManager_CancelTaskSemantics covers not-found, already-finished
// and not-interruptible cases; the last one must be reported explicitly rather
// than silently succeeding.
func TestSubagentManager_CancelTaskSemantics(t *testing.T) {
	mgr := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())

	if _, err := mgr.CancelTask("subagent-404"); !errors.Is(err, ErrSubagentTaskNotFound) {
		t.Errorf("unknown task err = %v, want ErrSubagentTaskNotFound", err)
	}

	handle, err := mgr.TrackSpawn(context.Background(), "t", "", "", "pico", "chat")
	if err != nil {
		t.Fatalf("TrackSpawn: %v", err)
	}
	handle.Finish(SubagentStatusCompleted, "done")
	if _, err := mgr.CancelTask(handle.ID); !errors.Is(err, ErrSubagentTaskNotRunning) {
		t.Errorf("finished task err = %v, want ErrSubagentTaskNotRunning", err)
	}

	// A task registered without a cancel handle is not interruptible.
	mgr.mu.Lock()
	mgr.tasks["subagent-manual"] = &SubagentTask{
		ID:     "subagent-manual",
		Status: SubagentStatusRunning,
	}
	mgr.mu.Unlock()
	if _, err := mgr.CancelTask("subagent-manual"); !errors.Is(err, ErrSubagentTaskNotInterruptible) {
		t.Errorf("non-cancelable task err = %v, want ErrSubagentTaskNotInterruptible", err)
	}
}

// TestSubagentManager_LegacySpawnIsInterruptible verifies the legacy
// manager.Spawn path also records a cancel handle (the spawner receives the
// cancelable child context).
func TestSubagentManager_LegacySpawnIsInterruptible(t *testing.T) {
	mgr := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())
	started := make(chan struct{})
	var once sync.Once
	mgr.SetSpawner(func(
		ctx context.Context,
		task, label, agentID string,
		tools *ToolRegistry,
		maxTokens int,
		temperature float64,
		hasMaxTokens, hasTemperature bool,
	) (*ToolResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	})

	msg, err := mgr.Spawn(context.Background(), "legacy job", "lbl", "", "pico", "chat-a", nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(msg, "已派生 subagent") {
		t.Errorf("Spawn message = %q", msg)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy spawn did not start")
	}
	tasks := mgr.ListTaskCopies()
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if _, err := mgr.CancelTask(tasks[0].ID); err != nil {
		t.Fatalf("CancelTask(legacy): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := mgr.GetTaskCopy(tasks[0].ID)
		if task.Status == SubagentStatusCanceled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := mgr.GetTaskCopy(tasks[0].ID)
	t.Fatalf("legacy task status = %q, want canceled", task.Status)
}
