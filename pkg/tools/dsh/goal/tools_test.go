package goal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return NewManager(dir)
}

func decodeGoal(t *testing.T, res interface{ ContentForLLM() string }) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.ContentForLLM()), &out); err != nil {
		t.Fatalf("decode tool result %q: %v", res.ContentForLLM(), err)
	}
	return out
}

func TestGoalCreateGetUpdatePersists(t *testing.T) {
	ws := t.TempDir()
	m := NewManager(ws)
	create := NewCreateGoalTool(m)
	res := create.Execute(context.Background(), map[string]any{"objective": "ship DSH goal tools"})
	env := decodeGoal(t, res)
	goal := env["goal"].(map[string]any)
	id, _ := goal["id"].(string)
	if !strings.HasPrefix(id, "goal-") || goal["status"] != StatusActive {
		t.Fatalf("unexpected created goal: %#v", goal)
	}

	get := NewGetGoalTool(m)
	env = decodeGoal(t, get.Execute(context.Background(), map[string]any{"id": id}))
	if env["goal"].(map[string]any)["objective"] != "ship DSH goal tools" {
		t.Fatalf("unexpected get result: %#v", env)
	}

	update := NewUpdateGoalTool(m)
	env = decodeGoal(t, update.Execute(context.Background(), map[string]any{
		"id": id, "status": StatusComplete, "note": "done", "evidence": []any{"go test ./pkg/tools/dsh/goal"},
	}))
	goal = env["goal"].(map[string]any)
	if goal["status"] != StatusComplete || goal["note"] != "done" {
		t.Fatalf("unexpected updated goal: %#v", goal)
	}

	// Persisted file must exist under state/goals with the id in the name.
	path := filepath.Join(ws, "state", "goals", id+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted goal file %s: %v", path, err)
	}

	// A fresh manager (restart) must see the same goal.
	m2 := NewManager(ws)
	env = decodeGoal(t, NewGetGoalTool(m2).Execute(context.Background(), map[string]any{"id": id}))
	if env["goal"].(map[string]any)["status"] != StatusComplete {
		t.Fatalf("goal not durable across managers: %#v", env)
	}

	// List mode returns one summary.
	env = decodeGoal(t, NewGetGoalTool(m2).Execute(context.Background(), map[string]any{}))
	if env["count"].(float64) != 1 || len(env["goals"].([]any)) != 1 {
		t.Fatalf("unexpected goal list: %#v", env)
	}
}

func TestGoalValidationAndTransitions(t *testing.T) {
	ws := t.TempDir()
	m := NewManager(ws)
	create := NewCreateGoalTool(m)
	env := decodeGoal(t, create.Execute(context.Background(), map[string]any{"objective": "x"}))
	id := env["goal"].(map[string]any)["id"].(string)
	update := NewUpdateGoalTool(m)

	// complete without note/evidence must fail.
	if res := update.Execute(context.Background(), map[string]any{"id": id, "status": StatusComplete}); res == nil || !res.IsError {
		t.Fatalf("expected complete-without-evidence to fail, got %+v", res)
	}
	// complete with evidence succeeds.
	env = decodeGoal(t, update.Execute(context.Background(), map[string]any{
		"id": id, "status": StatusComplete, "evidence": []any{"verified"},
	}))
	if env["goal"].(map[string]any)["status"] != StatusComplete {
		t.Fatalf("unexpected complete result: %#v", env)
	}
	// complete is terminal: further transition must fail.
	if res := update.Execute(context.Background(), map[string]any{"id": id, "status": StatusActive}); res == nil || !res.IsError {
		t.Fatalf("expected transition from complete to fail, got %+v", res)
	}
	// unknown id and invalid status.
	if res := update.Execute(context.Background(), map[string]any{"id": "goal-000001-deadbeef", "status": StatusActive}); res == nil || !res.IsError {
		t.Fatalf("expected unknown id to fail, got %+v", res)
	}
	if res := update.Execute(context.Background(), map[string]any{"id": id, "status": "nope"}); res == nil || !res.IsError {
		t.Fatalf("expected invalid status to fail, got %+v", res)
	}
}

func TestGoalCorruptFileSkipped(t *testing.T) {
	ws := t.TempDir()
	m := NewManager(ws)
	dir := filepath.Join(ws, "state", "goals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "goal-000001-badc0ffe.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := NewGetGoalTool(m).Execute(context.Background(), map[string]any{})
	env := decodeGoal(t, res)
	if skipped, _ := env["skipped"].([]any); len(skipped) != 1 {
		t.Fatalf("expected one skipped corrupt goal, got %#v", env)
	}
}

func TestGoalConcurrentCreateIsUnique(t *testing.T) {
	m := NewManager(t.TempDir())
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := NewCreateGoalTool(m).Execute(context.Background(), map[string]any{"objective": "c"})
			var env map[string]any
			if err := json.Unmarshal([]byte(res.ContentForLLM()), &env); err == nil {
				ids <- env["goal"].(map[string]any)["id"].(string)
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]struct{}{}
	for id := range ids {
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate goal id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 20 {
		t.Fatalf("created %d goals, want 20", len(seen))
	}
}

func TestGoalNilManager(t *testing.T) {
	if res := NewCreateGoalTool(nil).Execute(context.Background(), map[string]any{"objective": "x"}); res == nil || !res.IsError {
		t.Fatalf("expected nil manager error, got %+v", res)
	}
	if res := NewCreateGoalTool(NewManager("")).Execute(context.Background(), map[string]any{"objective": "x"}); res == nil || !res.IsError {
		t.Fatalf("expected empty workspace error, got %+v", res)
	}
}
