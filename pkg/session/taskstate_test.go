package session

import (
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/memory"
	"github.com/rlds/chagent/pkg/taskstate"
)

func newSessionTestTaskState(t *testing.T, objective string) taskstate.State {
	t.Helper()
	state, err := taskstate.New(objective, 20, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("taskstate.New: %v", err)
	}
	return state
}

func TestSessionStoreImplementsTaskStateStore(t *testing.T) {
	var jsonl SessionStore = NewJSONLBackend(nil)
	if _, ok := jsonl.(TaskStateStore); !ok {
		t.Fatal("JSONLBackend should implement TaskStateStore")
	}
	var legacy SessionStore = NewSessionManager("")
	if _, ok := legacy.(TaskStateStore); !ok {
		t.Fatal("SessionManager should implement TaskStateStore")
	}
}

func TestTaskStateStoreLegacyManager(t *testing.T) {
	dir := t.TempDir()
	manager := NewSessionManager(dir)

	key := "agent:legacy:chat"
	if _, ok := manager.GetTaskState(key); ok {
		t.Fatal("GetTaskState on empty manager should return false")
	}

	state := newSessionTestTaskState(t, "legacy 目标")
	state.Todos = []taskstate.Todo{{Content: "步骤", Status: taskstate.TodoPending}}
	if err := manager.SetTaskState(key, state); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
	manager.AddMessage(key, "user", "你好")
	if err := manager.Save(key); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, ok := manager.GetTaskState(key)
	if !ok || loaded.Objective != "legacy 目标" || len(loaded.Todos) != 1 {
		t.Fatalf("GetTaskState = %+v, ok=%v", loaded, ok)
	}
	loaded.Todos[0].Content = "mutated"
	if again, _ := manager.GetTaskState(key); again.Todos[0].Content != "步骤" {
		t.Fatal("GetTaskState must return a deep copy")
	}

	// Reload from disk (simulated restart).
	reloaded := NewSessionManager(dir)
	fromDisk, ok := reloaded.GetTaskState(key)
	if !ok || fromDisk.Objective != "legacy 目标" || fromDisk.Round != 0 {
		t.Fatalf("task state did not persist: %+v, ok=%v", fromDisk, ok)
	}
	if len(reloaded.GetHistory(key)) != 1 {
		t.Fatalf("history lost after reload: %d", len(reloaded.GetHistory(key)))
	}

	// Zero state clears.
	if err := reloaded.SetTaskState(key, taskstate.State{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := reloaded.GetTaskState(key); ok {
		t.Fatal("zero state should clear task state")
	}
}

func TestTaskStateStoreJSONLBackend(t *testing.T) {
	memStore, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	backend := NewJSONLBackend(memStore)

	key := "agent:jsonl:chat"
	state := newSessionTestTaskState(t, "jsonl 目标")
	state.Round = 3
	if err := backend.SetTaskState(key, state); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}

	loaded, ok := backend.GetTaskState(key)
	if !ok || loaded.Objective != "jsonl 目标" || loaded.Round != 3 {
		t.Fatalf("GetTaskState = %+v, ok=%v", loaded, ok)
	}

	// State is visible through the store directly too.
	direct, ok := memStore.GetTaskState(key)
	if !ok || direct.Objective != "jsonl 目标" {
		t.Fatalf("memory store GetTaskState = %+v, ok=%v", direct, ok)
	}

	if err := backend.SetTaskState(key, taskstate.State{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := backend.GetTaskState(key); ok {
		t.Fatal("zero state should clear task state")
	}

	invalid := newSessionTestTaskState(t, "无效")
	invalid.MaxRounds = 0
	if err := backend.SetTaskState(key, invalid); err == nil {
		t.Fatal("SetTaskState should propagate validation errors")
	}
}
