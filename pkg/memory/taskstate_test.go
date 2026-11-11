package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/taskstate"
)

func newTestTaskState(t *testing.T, objective string) taskstate.State {
	t.Helper()
	state, err := taskstate.New(objective, 20, time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("taskstate.New: %v", err)
	}
	return state
}

func TestJSONLTaskStatePersist(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	key := "agent:main:pico-user"
	if _, ok := store.GetTaskState(key); ok {
		t.Fatal("GetTaskState on empty store should return false")
	}

	state := newTestTaskState(t, "重构 provider 层")
	state.Todos = []taskstate.Todo{
		{Content: "盘点接口", Status: taskstate.TodoCompleted},
		{Content: "迁移适配器", Status: taskstate.TodoInProgress},
	}
	state.Round = 2
	state.Evidence = []string{"go test ./pkg/providers/... 通过"}
	if err := store.SetTaskState(key, state); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}

	loaded, ok := store.GetTaskState(key)
	if !ok {
		t.Fatal("GetTaskState after SetTaskState should return true")
	}
	if loaded.Objective != state.Objective || loaded.Round != 2 || len(loaded.Todos) != 2 ||
		len(loaded.Evidence) != 1 || loaded.Phase != taskstate.PhaseActive {
		t.Fatalf("loaded state mismatch: %+v", loaded)
	}

	// Mutating the returned value must not corrupt the store.
	loaded.Todos[0].Content = "mutated"
	if again, _ := store.GetTaskState(key); again.Todos[0].Content != "盘点接口" {
		t.Fatal("GetTaskState must return a deep copy")
	}

	// Session history and metadata stay usable with a task state attached.
	if err := store.AddMessage(context.Background(), key, "user", "你好"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	history, err := store.GetHistory(context.Background(), key)
	if err != nil || len(history) != 1 {
		t.Fatalf("GetHistory = %d messages, err=%v", len(history), err)
	}

	// A new store instance (simulated restart) can read the persisted state.
	restarted, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore(restart): %v", err)
	}
	reloaded, ok := restarted.GetTaskState(key)
	if !ok || reloaded.Objective != "重构 provider 层" || reloaded.Round != 2 {
		t.Fatalf("state did not survive restart: %+v, ok=%v", reloaded, ok)
	}

	// Writing a zero state clears the stored task state.
	if err := store.SetTaskState(key, taskstate.State{}); err != nil {
		t.Fatalf("clear TaskState: %v", err)
	}
	if _, ok := store.GetTaskState(key); ok {
		t.Fatal("zero state should clear stored task state")
	}
	// Clearing the task state must not remove session history.
	history, err = store.GetHistory(context.Background(), key)
	if err != nil || len(history) != 1 {
		t.Fatalf("history lost after clearing task state: %d, err=%v", len(history), err)
	}
}

func TestJSONLTaskStateRejectsInvalidState(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	invalid := newTestTaskState(t, "目标")
	invalid.MaxRounds = 0
	if err := store.SetTaskState("s1", invalid); err == nil {
		t.Fatal("SetTaskState should reject invalid state")
	}
}

func TestLegacyMetaWithoutTaskState(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}

	key := "agent:legacy:chat"
	// Write a legacy meta file by hand: no task_state key at all.
	legacy := map[string]any{
		"key":        key,
		"summary":    "旧摘要",
		"skip":       0,
		"count":      1,
		"created_at": "2026-10-01T00:00:00Z",
		"updated_at": "2026-10-01T00:00:00Z",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sanitizeKey(key)+".meta.json"), data, 0o644); err != nil {
		t.Fatalf("write legacy meta: %v", err)
	}

	meta, err := store.GetSessionMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if meta.TaskState != nil {
		t.Fatalf("legacy meta should load with nil TaskState, got %+v", meta.TaskState)
	}
	if _, ok := store.GetTaskState(key); ok {
		t.Fatal("legacy meta should report no task state")
	}

	// Attaching a task state to a legacy session works and preserves the summary.
	state := newTestTaskState(t, "新目标")
	if err := store.SetTaskState(key, state); err != nil {
		t.Fatalf("SetTaskState on legacy meta: %v", err)
	}
	meta, err = store.GetSessionMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("GetSessionMeta after set: %v", err)
	}
	if meta.Summary != "旧摘要" {
		t.Fatalf("summary = %q, want 旧摘要", meta.Summary)
	}
	if meta.TaskState == nil || meta.TaskState.Objective != "新目标" {
		t.Fatalf("TaskState = %+v", meta.TaskState)
	}
}

func TestTaskStateMetaJSONShape(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	key := "s1"
	state := newTestTaskState(t, "目标")
	if err := store.SetTaskState(key, state); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, sanitizeKey(key)+".meta.json"))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := raw["task_state"]; !ok {
		t.Fatalf("meta file missing task_state: %s", data)
	}
	var decoded taskstate.State
	if err := json.Unmarshal(raw["task_state"], &decoded); err != nil {
		t.Fatalf("decode task_state: %v", err)
	}
	if decoded.Objective != "目标" || decoded.MaxRounds != 20 {
		t.Fatalf("decoded task_state = %+v", decoded)
	}
}
