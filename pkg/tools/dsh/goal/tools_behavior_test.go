package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

func newToolSet(t *testing.T) (*Manager, *CreateGoalTool, *GetGoalTool, *UpdateGoalTool) {
	t.Helper()
	m := NewManager(t.TempDir())
	return m, NewCreateGoalTool(m), NewGetGoalTool(m), NewUpdateGoalTool(m)
}

func execTool(t *testing.T, tool toolshared.Tool, args map[string]any) *toolshared.ToolResult {
	t.Helper()
	return tool.Execute(context.Background(), args)
}

func decodeGoalView(t *testing.T, result *toolshared.ToolResult) goalView {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.ForLLM)
	}
	var envelope goalEnvelope
	if err := json.Unmarshal([]byte(result.ForLLM), &envelope); err != nil {
		t.Fatalf("decode goal envelope: %v\n%s", err, result.ForLLM)
	}
	if envelope.Goal == nil {
		t.Fatalf("goal envelope has nil goal: %s", result.ForLLM)
	}
	return *envelope.Goal
}

func decodeListEnvelope(t *testing.T, result *toolshared.ToolResult) goalListEnvelope {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.ForLLM)
	}
	var envelope goalListEnvelope
	if err := json.Unmarshal([]byte(result.ForLLM), &envelope); err != nil {
		t.Fatalf("decode list envelope: %v\n%s", err, result.ForLLM)
	}
	return envelope
}

func TestToolNamesDescriptionsAndParameters(t *testing.T) {
	_, create, get, update := newToolSet(t)
	cases := []struct {
		tool toolshared.Tool
		name string
		keys []string
		req  []string
	}{
		{create, "create_goal", []string{"objective", "note"}, []string{"objective"}},
		{get, "get_goal", []string{"id", "limit"}, nil},
		{update, "update_goal", []string{"id", "status", "note", "evidence"}, []string{"id"}},
	}
	for _, tc := range cases {
		if got := tc.tool.Name(); got != tc.name {
			t.Fatalf("Name() = %q, want %q", got, tc.name)
		}
		if desc := tc.tool.Description(); desc == "" {
			t.Fatalf("%s Description() is empty", tc.name)
		}
		params := tc.tool.Parameters()
		if params["type"] != "object" {
			t.Fatalf("%s Parameters type = %v", tc.name, params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s Parameters missing properties", tc.name)
		}
		for _, key := range tc.keys {
			if _, ok := props[key]; !ok {
				t.Fatalf("%s Parameters missing property %q", tc.name, key)
			}
		}
		if tc.req != nil {
			required, _ := params["required"].([]string)
			if strings.Join(required, ",") != strings.Join(tc.req, ",") {
				t.Fatalf("%s required = %v, want %v", tc.name, required, tc.req)
			}
		}
	}
}

func TestPromptMetadata(t *testing.T) {
	_, create, get, update := newToolSet(t)
	want := toolshared.PromptMetadata{Layer: "capability", Slot: "dsh", Source: "tool_registry:dsh"}
	for _, tool := range []toolshared.Tool{create, get, update} {
		provider, ok := tool.(toolshared.PromptMetadataProvider)
		if !ok {
			t.Fatalf("%s does not implement PromptMetadataProvider", tool.Name())
		}
		if got := provider.PromptMetadata(); got != want {
			t.Fatalf("%s PromptMetadata = %+v, want %+v", tool.Name(), got, want)
		}
	}
}

func TestRegister(t *testing.T) {
	registry := tools.NewToolRegistry()
	m := NewManager(t.TempDir())
	Register(registry, m)

	for _, name := range []string{"create_goal", "get_goal", "update_goal"} {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("registry missing %q after Register", name)
		}
		if tool.Name() != name {
			t.Fatalf("registered tool Name() = %q, want %q", tool.Name(), name)
		}
	}
	if got := len(registry.List()); got != 3 {
		t.Fatalf("registry size = %d, want 3", got)
	}

	Register(nil, m)        // nil registry is a no-op
	Register(registry, nil) // nil manager is a no-op
	if got := len(registry.List()); got != 3 {
		t.Fatalf("registry changed by no-op Register: %d tools", got)
	}
}

func TestCreateGetUpdateThroughTools(t *testing.T) {
	_, create, get, update := newToolSet(t)

	created := decodeGoalView(t, execTool(t, create, map[string]any{"objective": "完成 goal 工具重写", "note": "初始"}))
	if created.ID == "" || created.Status != StatusActive {
		t.Fatalf("created = %+v", created)
	}
	if created.Objective != "完成 goal 工具重写" || created.Note != "初始" {
		t.Fatalf("created content = %+v", created)
	}

	fetched := decodeGoalView(t, execTool(t, get, map[string]any{"id": created.ID}))
	if fetched.ID != created.ID || fetched.Objective != created.Objective {
		t.Fatalf("fetched = %+v, want %+v", fetched, created)
	}

	list := decodeListEnvelope(t, execTool(t, get, map[string]any{}))
	if list.Count != 1 || list.Truncated || len(list.Goals) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list.Goals[0].ID != created.ID || list.Goals[0].Note != "初始" {
		t.Fatalf("list goal = %+v", list.Goals[0])
	}

	updated := decodeGoalView(t, execTool(t, update, map[string]any{
		"id":       created.ID,
		"status":   StatusComplete,
		"evidence": []any{"go test ./pkg/tools/dsh/goal/... PASS"},
	}))
	if updated.Status != StatusComplete {
		t.Fatalf("updated status = %q", updated.Status)
	}
	if len(updated.Evidence) != 1 || !strings.Contains(updated.Evidence[0], "PASS") {
		t.Fatalf("updated evidence = %#v", updated.Evidence)
	}
}

func TestUpdateLifecycleThroughTools(t *testing.T) {
	_, create, _, update := newToolSet(t)
	goal := decodeGoalView(t, execTool(t, create, map[string]any{"objective": "目标"}))

	result := execTool(t, update, map[string]any{"id": goal.ID, "status": StatusBlocked})
	if !result.IsError || !strings.Contains(result.ForLLM, "goal_missing_evidence") {
		t.Fatalf("blocked without note/evidence = %q, want goal_missing_evidence", result.ForLLM)
	}
	blocked := decodeGoalView(t, execTool(t, update, map[string]any{"id": goal.ID, "status": StatusBlocked, "note": "缺少外部凭据"}))
	if blocked.Status != StatusBlocked {
		t.Fatalf("status = %q", blocked.Status)
	}
	reopened := decodeGoalView(t, execTool(t, update, map[string]any{"id": goal.ID, "status": StatusActive}))
	if reopened.Status != StatusActive {
		t.Fatalf("reopened status = %q", reopened.Status)
	}
	completed := decodeGoalView(t, execTool(t, update, map[string]any{"id": goal.ID, "status": StatusComplete, "evidence": []any{"done"}}))
	if completed.Status != StatusComplete {
		t.Fatalf("completed status = %q", completed.Status)
	}
	result = execTool(t, update, map[string]any{"id": goal.ID, "status": StatusActive})
	if !result.IsError || !strings.Contains(result.ForLLM, "goal_terminal") {
		t.Fatalf("complete->active = %q, want goal_terminal", result.ForLLM)
	}
}

func TestToolArgumentDiagnostics(t *testing.T) {
	_, create, get, update := newToolSet(t)

	if result := execTool(t, create, map[string]any{}); !result.IsError || !strings.Contains(result.ForLLM, "goal_invalid_argument") {
		t.Fatalf("create without objective = %q", result.ForLLM)
	}
	if result := execTool(t, create, map[string]any{"objective": 42}); !result.IsError || !strings.Contains(result.ForLLM, "objective 必须是字符串") {
		t.Fatalf("create with non-string objective = %q", result.ForLLM)
	}
	if result := execTool(t, get, map[string]any{"id": "goal-000001-deadbeef"}); !result.IsError ||
		!strings.Contains(result.ForLLM, "goal_not_found") {
		t.Fatalf("get unknown = %q", result.ForLLM)
	}
	if result := execTool(t, get, map[string]any{"limit": "many"}); !result.IsError ||
		!strings.Contains(result.ForLLM, "limit 必须是整数") {
		t.Fatalf("get bad limit = %q", result.ForLLM)
	}
	if result := execTool(t, update, map[string]any{"status": StatusActive}); !result.IsError ||
		!strings.Contains(result.ForLLM, "需要非空 id") {
		t.Fatalf("update without id = %q", result.ForLLM)
	}
	if result := execTool(t, update, map[string]any{"id": "goal-000001-deadbeef"}); !result.IsError ||
		!strings.Contains(result.ForLLM, "goal_invalid_argument") {
		t.Fatalf("update without fields = %q", result.ForLLM)
	}
	if result := execTool(t, update, map[string]any{"id": "../escape", "status": StatusActive}); !result.IsError ||
		!strings.Contains(result.ForLLM, "goal_invalid_id") {
		t.Fatalf("update path escape = %q", result.ForLLM)
	}
	if result := execTool(t, update, map[string]any{"id": "goal-000001-deadbeef", "status": "done"}); !result.IsError ||
		!strings.Contains(result.ForLLM, "goal_invalid_status") {
		t.Fatalf("update invalid status = %q", result.ForLLM)
	}
}

func TestNilManagerTools(t *testing.T) {
	for _, tool := range []toolshared.Tool{
		NewCreateGoalTool(nil),
		NewGetGoalTool(nil),
		NewUpdateGoalTool(nil),
	} {
		result := tool.Execute(context.Background(), map[string]any{"objective": "x", "id": "goal-000001-deadbeef"})
		if !result.IsError || !strings.Contains(result.ForLLM, "goal manager 未初始化") {
			t.Fatalf("%s nil manager result = %q", tool.Name(), result.ForLLM)
		}
	}
	empty := NewManager("")
	result := NewCreateGoalTool(empty).Execute(context.Background(), map[string]any{"objective": "x"})
	if !result.IsError || !strings.Contains(result.ForLLM, "goal manager 未初始化") {
		t.Fatalf("empty workspace tool result = %q", result.ForLLM)
	}
}

func TestGetGoalListLimit(t *testing.T) {
	m, _, get, _ := newToolSet(t)
	for i := 0; i < 23; i++ {
		if _, err := m.Create(fmt.Sprintf("objective-%02d", i), ""); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	limited := decodeListEnvelope(t, execTool(t, get, map[string]any{"limit": 5}))
	if limited.Count != 23 || !limited.Truncated || len(limited.Goals) != 5 {
		t.Fatalf("limited list = %+v", limited)
	}
	if limited.Limit != 5 {
		t.Fatalf("limit = %d, want 5", limited.Limit)
	}

	capped := decodeListEnvelope(t, execTool(t, get, map[string]any{"limit": 999}))
	if capped.Limit != ListLimit {
		t.Fatalf("limit = %d, want cap %d", capped.Limit, ListLimit)
	}
	if capped.Count != 23 || capped.Truncated {
		t.Fatalf("capped list = %+v", capped)
	}

	defaulted := decodeListEnvelope(t, execTool(t, get, map[string]any{}))
	if defaulted.Limit != defaultListLimit {
		t.Fatalf("default limit = %d, want %d", defaulted.Limit, defaultListLimit)
	}
	if defaulted.Count != 23 || !defaulted.Truncated || len(defaulted.Goals) != defaultListLimit {
		t.Fatalf("default list = %+v", defaulted)
	}
}

func TestGetGoalListReportsCorruptFiles(t *testing.T) {
	m, _, get, _ := newToolSet(t)
	if _, err := m.Create("objective", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir(), "goal-999999-deadbeef.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := decodeListEnvelope(t, execTool(t, get, map[string]any{}))
	if list.Count != 1 || len(list.Goals) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if len(list.Skipped) != 1 || !strings.Contains(list.Skipped[0].Reason, "goal_corrupt") {
		t.Fatalf("skipped = %#v, want one goal_corrupt diagnostic", list.Skipped)
	}
}

func TestConcurrentToolUse(t *testing.T) {
	_, create, get, _ := newToolSet(t)
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result := execTool(t, create, map[string]any{"objective": fmt.Sprintf("concurrent-%d", index)})
			if result.IsError {
				t.Errorf("create %d: %s", index, result.ForLLM)
			}
		}(i)
	}
	wg.Wait()
	list := decodeListEnvelope(t, get.Execute(context.Background(), map[string]any{"limit": ListLimit}))
	if list.Count != workers {
		t.Fatalf("count = %d, want %d", list.Count, workers)
	}
	if len(list.Skipped) != 0 {
		t.Fatalf("skipped = %#v", list.Skipped)
	}
}

func TestDecodeResultShapeIsJSONOrError(t *testing.T) {
	_, create, get, update := newToolSet(t)
	for _, result := range []*toolshared.ToolResult{
		execTool(t, create, map[string]any{"objective": "目标"}),
		execTool(t, get, map[string]any{}),
		execTool(t, update, map[string]any{"id": "goal-000001-deadbeef", "note": "x"}),
	} {
		if result.IsError {
			if result.ForLLM == "" {
				t.Fatalf("error result has empty ForLLM")
			}
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(result.ForLLM), &decoded); err != nil {
			t.Fatalf("non-error result is not JSON: %v\n%s", err, result.ForLLM)
		}
		if _, ok := decoded["goal"]; !ok {
			if _, listOK := decoded["goals"]; !listOK {
				t.Fatalf("non-error result missing goal/goals key: %s", result.ForLLM)
			}
		}
	}
}
