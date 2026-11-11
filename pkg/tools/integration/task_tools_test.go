package integrationtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/taskstate"
)

type taskTestEnv struct {
	manager *session.SessionManager
	get     *TaskGetTool
	update  *TaskUpdateTool
	todo    *TodoWriteTool
	key     string
	now     time.Time
}

func newTaskTestEnv(t *testing.T, tweak func(*TaskToolOptions)) *taskTestEnv {
	t.Helper()
	manager := session.NewSessionManager("")
	env := &taskTestEnv{
		manager: manager,
		key:     "agent:main:task-test",
		now:     time.Date(2026, 10, 28, 16, 0, 0, 0, time.FixedZone("CST", 8*3600)),
	}
	opts := TaskToolOptions{
		Sessions: manager,
		Now:      func() time.Time { return env.now },
	}
	if tweak != nil {
		tweak(&opts)
	}
	env.get = NewTaskGetTool(opts)
	env.update = NewTaskUpdateTool(opts)
	env.todo = NewTodoWriteTool(opts)
	return env
}

func (e *taskTestEnv) ctx(source Source) context.Context {
	ctx := WithToolSessionContext(context.Background(), "main", e.key, nil)
	return WithTurnSource(ctx, source)
}

func (e *taskTestEnv) humanCtx() context.Context { return e.ctx(SourceHuman) }

func (e *taskTestEnv) execUpdate(t *testing.T, ctx context.Context, args map[string]any) *ToolResult {
	t.Helper()
	return e.update.Execute(ctx, args)
}

func (e *taskTestEnv) requireState(t *testing.T) taskstate.State {
	t.Helper()
	state, ok := e.manager.GetTaskState(e.key)
	if !ok {
		t.Fatal("expected a task state")
	}
	return state
}

func (e *taskTestEnv) setState(t *testing.T, state taskstate.State) {
	t.Helper()
	if err := e.manager.SetTaskState(e.key, state); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
}

func (e *taskTestEnv) start(t *testing.T) taskstate.State {
	t.Helper()
	result := e.execUpdate(t, e.humanCtx(), map[string]any{
		"action":    "start",
		"objective": "重构 provider 层并保持所有测试通过",
	})
	if result.IsError {
		t.Fatalf("start failed: %s", result.ForLLM)
	}
	return e.requireState(t)
}

func TestTaskGetNoState(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	result := env.get.Execute(env.humanCtx(), map[string]any{})
	if result.IsError {
		t.Fatalf("task_get without state should not be an error: %s", result.ForLLM)
	}
	if strings.TrimSpace(result.ForLLM) != `{"task":null}` {
		t.Fatalf("task_get result = %q, want {\"task\":null}", result.ForLLM)
	}
}

func TestTaskGetStableJSON(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)

	getResult := env.get.Execute(env.humanCtx(), map[string]any{})
	var payload struct {
		Task *struct {
			Objective     string                   `json:"objective"`
			Phase         string                   `json:"phase"`
			Round         int                      `json:"round"`
			MaxRounds     int                      `json:"max_rounds"`
			Todos         []taskstate.Todo         `json:"todos"`
			Evidence      []string                 `json:"evidence"`
			BlockedReason *taskstate.BlockedReason `json:"blocked_reason"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(getResult.ForLLM), &payload); err != nil {
		t.Fatalf("task_get result is not JSON: %v\n%s", err, getResult.ForLLM)
	}
	if payload.Task == nil {
		t.Fatal("task should not be nil after start")
	}
	if payload.Task.Phase != "active" || payload.Task.MaxRounds != 20 ||
		payload.Task.Objective != "重构 provider 层并保持所有测试通过" {
		t.Fatalf("unexpected task payload: %+v", payload.Task)
	}
	if payload.Task.Todos == nil || payload.Task.Evidence == nil {
		t.Fatalf("todos/evidence must be arrays, got %+v", payload.Task)
	}
	if payload.Task.BlockedReason != nil {
		t.Fatalf("blocked_reason = %+v, want null", payload.Task.BlockedReason)
	}
}

func TestTaskStartRequiresHuman(t *testing.T) {
	for _, source := range []Source{SourceAutonomy, SourceSubagent, SourceSystem, SourceUnknown} {
		env := newTaskTestEnv(t, nil)
		result := env.execUpdate(t, env.ctx(source), map[string]any{
			"action":    "start",
			"objective": "目标",
		})
		if !result.IsError {
			t.Fatalf("start from source %s should be rejected", source)
		}
		if !strings.Contains(result.ForLLM, "需要直接的人类或调度器请求") {
			t.Fatalf("start rejection message = %q", result.ForLLM)
		}
		if _, ok := env.manager.GetTaskState(env.key); ok {
			t.Fatalf("start from source %s must not create task state", source)
		}
	}

	// Scheduler (heartbeat/cron) may start a task.
	schedulerEnv := newTaskTestEnv(t, nil)
	result := schedulerEnv.execUpdate(t, schedulerEnv.ctx(SourceScheduler), map[string]any{
		"action":    "start",
		"objective": "调度任务",
	})
	if result.IsError {
		t.Fatalf("scheduler start should succeed: %s", result.ForLLM)
	}

	// Human may start a task.
	humanEnv := newTaskTestEnv(t, nil)
	if result := humanEnv.execUpdate(t, humanEnv.humanCtx(), map[string]any{
		"action":    "start",
		"objective": "人类任务",
	}); result.IsError {
		t.Fatalf("human start should succeed: %s", result.ForLLM)
	}

	// Missing objective is rejected.
	missingObjective := newTaskTestEnv(t, nil)
	if result := missingObjective.execUpdate(t, missingObjective.humanCtx(), map[string]any{
		"action": "start",
	}); !result.IsError {
		t.Fatal("start without objective should be rejected")
	}
}

func TestTaskUpdatePhaseTransitions(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	state := env.start(t)
	if state.Phase != taskstate.PhaseActive || state.Round != 0 || state.MaxRounds != taskstate.DefaultMaxRounds {
		t.Fatalf("start state = %+v", state)
	}

	// active → paused
	result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "pause"})
	if result.IsError {
		t.Fatalf("pause: %s", result.ForLLM)
	}
	if got := env.requireState(t); got.Phase != taskstate.PhasePaused {
		t.Fatalf("phase = %s, want paused", got.Phase)
	}
	// pausing twice fails
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "pause"}); !result.IsError {
		t.Fatal("pausing twice should fail")
	}

	// paused → active
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "resume"}); result.IsError {
		t.Fatalf("resume: %s", result.ForLLM)
	}
	if got := env.requireState(t); got.Phase != taskstate.PhaseActive {
		t.Fatalf("phase = %s, want active", got.Phase)
	}
	// resuming an active task fails
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "resume"}); !result.IsError {
		t.Fatal("resuming an active task should fail")
	}

	// active → complete (needs evidence)
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":   "complete",
		"evidence": []any{"go test ./pkg/... 通过"},
	}); result.IsError {
		t.Fatalf("complete: %s", result.ForLLM)
	}
	if got := env.requireState(t); got.Phase != taskstate.PhaseComplete || len(got.Evidence) != 1 {
		t.Fatalf("complete state = %+v", got)
	}
	// complete is terminal
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "resume"}); !result.IsError {
		t.Fatal("resuming a complete task should fail")
	}
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "pause"}); !result.IsError {
		t.Fatal("pausing a complete task should fail")
	}

	// A new task may start after completion.
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":    "start",
		"objective": "第二个任务",
	}); result.IsError {
		t.Fatalf("start after complete: %s", result.ForLLM)
	}

	// Starting while a task is active is rejected.
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":    "start",
		"objective": "第三个任务",
	}); !result.IsError {
		t.Fatal("start while active should fail")
	}
}

func TestTaskCompleteRequiresEvidence(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)

	for _, evidence := range [][]any{
		nil,
		{"   "},
		{},
	} {
		result := env.execUpdate(t, env.humanCtx(), map[string]any{
			"action":   "complete",
			"evidence": evidence,
		})
		if !result.IsError {
			t.Fatalf("complete with evidence %v should fail", evidence)
		}
		if !strings.Contains(result.ForLLM, "证据") {
			t.Fatalf("evidence error message = %q", result.ForLLM)
		}
	}
	if got := env.requireState(t); got.Phase != taskstate.PhaseActive {
		t.Fatalf("failed completion should keep active, got %s", got.Phase)
	}

	result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":   "complete",
		"evidence": []any{"pkg/taskstate/taskstate_test.go", "go test ./pkg/taskstate/ 通过"},
	})
	if result.IsError {
		t.Fatalf("complete with evidence: %s", result.ForLLM)
	}
	got := env.requireState(t)
	if got.Phase != taskstate.PhaseComplete || len(got.Evidence) != 2 {
		t.Fatalf("complete state = %+v", got)
	}
	if !strings.Contains(result.ForLLM, "已记录 2 条证据") {
		t.Fatalf("complete result = %q", result.ForLLM)
	}
}

func TestTaskBlockThreshold(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)
	reason := "缺少 chess 服务端签发的 pico token"

	// Attempt 1 (round 1): rejected, counter starts at 1.
	state := env.requireState(t)
	state.Round = 1
	env.setState(t, state)
	result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":         "block",
		"blocked_reason": reason,
	})
	if !result.IsError || !strings.Contains(result.ForLLM, "连续至少 3 轮") {
		t.Fatalf("round 1 block should be rejected: %s", result.ForLLM)
	}
	state = env.requireState(t)
	if state.BlockerRounds != 1 || state.Phase != taskstate.PhaseActive {
		t.Fatalf("round 1 state = %+v", state)
	}

	// Repeating in the same round must not advance the counter.
	result = env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":         "block",
		"blocked_reason": reason,
	})
	if !result.IsError {
		t.Fatal("same-round repeat should be rejected")
	}
	if state = env.requireState(t); state.BlockerRounds != 1 {
		t.Fatalf("same-round repeat advanced counter to %d", state.BlockerRounds)
	}

	// Round 2: still rejected.
	state.Round = 2
	env.setState(t, state)
	if result = env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":         "block",
		"blocked_reason": reason,
	}); !result.IsError {
		t.Fatal("round 2 block should be rejected")
	}
	if state = env.requireState(t); state.BlockerRounds != 2 {
		t.Fatalf("round 2 counter = %d", state.BlockerRounds)
	}

	// A different condition resets the counter.
	if result = env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":         "block",
		"blocked_reason": "另一个完全不同的条件",
	}); !result.IsError {
		t.Fatal("different condition should be rejected")
	}
	if state = env.requireState(t); state.BlockerRounds != 1 {
		t.Fatalf("different condition should reset counter, got %d", state.BlockerRounds)
	}

	// Re-report the original condition in rounds 3 and 4.
	for _, round := range []int{3, 4, 5} {
		state = env.requireState(t)
		state.Round = round
		env.setState(t, state)
		result = env.execUpdate(t, env.humanCtx(), map[string]any{
			"action":         "block",
			"blocked_reason": reason,
		})
	}
	if result.IsError {
		t.Fatalf("round 5 block should succeed: %s", result.ForLLM)
	}
	state = env.requireState(t)
	if state.Phase != taskstate.PhaseBlocked || state.BlockedReason == nil {
		t.Fatalf("blocked state = %+v", state)
	}
	if state.BlockedReason.Code != taskstate.BlockedCodeModelReported ||
		state.BlockedReason.Message != reason {
		t.Fatalf("blocked reason = %+v", state.BlockedReason)
	}
}

func TestTaskBlockRequiresReason(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)
	result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "block"})
	if !result.IsError || !strings.Contains(result.ForLLM, "blocked_reason") {
		t.Fatalf("block without reason = %q", result.ForLLM)
	}
}

func TestTaskResumeBlockedRequiresHuman(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	state := env.start(t)
	state.Round = 3
	state.BlockerFingerprint = (taskstate.BlockedReason{Code: "model-reported", Message: "条件"}).Fingerprint()
	state.BlockerRounds = 3
	state.BlockerLastRound = 3
	state.Phase = taskstate.PhaseBlocked
	state.BlockedReason = &taskstate.BlockedReason{Code: "model-reported", Message: "条件"}
	env.setState(t, state)

	result := env.execUpdate(t, env.ctx(SourceAutonomy), map[string]any{"action": "resume"})
	if !result.IsError || !strings.Contains(result.ForLLM, "需要直接的人类请求") {
		t.Fatalf("autonomy resume of blocked = %q", result.ForLLM)
	}
	if env.requireState(t).Phase != taskstate.PhaseBlocked {
		t.Fatal("blocked task should stay blocked after rejected resume")
	}

	result = env.execUpdate(t, env.humanCtx(), map[string]any{"action": "resume"})
	if result.IsError {
		t.Fatalf("human resume of blocked should succeed: %s", result.ForLLM)
	}
	got := env.requireState(t)
	if got.Phase != taskstate.PhaseActive || got.BlockedReason != nil || got.NoProgressRounds != 0 {
		t.Fatalf("resumed state = %+v", got)
	}
}

func TestTodoWriteReplace(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)

	result := env.todo.Execute(env.humanCtx(), map[string]any{
		"todos": []any{
			map[string]any{"content": "步骤一", "status": "completed"},
			map[string]any{"content": "步骤二", "status": "in_progress"},
			map[string]any{"content": "步骤三", "status": "pending"},
		},
	})
	if result.IsError {
		t.Fatalf("todo_write: %s", result.ForLLM)
	}
	const want = "任务清单已更新：1 项待处理，1 项进行中，1 项已完成。"
	if strings.TrimSpace(result.ForLLM) != want {
		t.Fatalf("todo_write result = %q, want %q", result.ForLLM, want)
	}
	if got := env.requireState(t); len(got.Todos) != 3 {
		t.Fatalf("todos = %+v", got.Todos)
	}

	// Second call replaces the whole list.
	result = env.todo.Execute(env.humanCtx(), map[string]any{
		"todos": []any{
			map[string]any{"content": "新步骤", "status": "pending"},
		},
	})
	if result.IsError {
		t.Fatalf("todo_write replace: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "1 项待处理，0 项进行中，0 项已完成") {
		t.Fatalf("replace result = %q", result.ForLLM)
	}
	todos := env.requireState(t).Todos
	if len(todos) != 1 || todos[0].Content != "新步骤" {
		t.Fatalf("replaced todos = %+v", todos)
	}
}

func TestTodoWriteDuplicateRejected(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)

	cases := [][]any{
		{
			map[string]any{"content": "重复", "status": "pending"},
			map[string]any{"content": " 重复 ", "status": "completed"},
		},
		{
			map[string]any{"content": "  ", "status": "pending"},
		},
		{
			map[string]any{"content": "无效状态", "status": "done"},
		},
	}
	for i, todos := range cases {
		result := env.todo.Execute(env.humanCtx(), map[string]any{"todos": todos})
		if !result.IsError {
			t.Fatalf("case %d should be rejected", i)
		}
		if !strings.Contains(result.ForLLM, "无效待办") {
			t.Fatalf("case %d error = %q", i, result.ForLLM)
		}
	}
	if got := env.requireState(t); len(got.Todos) != 0 {
		t.Fatalf("invalid todo_write must not change todos: %+v", got.Todos)
	}
}

func TestTodoWriteParallelPolicy(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	env.start(t)

	parallel := []any{
		map[string]any{"content": "并行一", "status": "in_progress"},
		map[string]any{"content": "并行二", "status": "in_progress"},
	}
	result := env.todo.Execute(env.humanCtx(), map[string]any{"todos": parallel})
	if !result.IsError || !strings.Contains(result.ForLLM, "最多只能有一个 in_progress（当前 2 个）") {
		t.Fatalf("parallel rejection = %q", result.ForLLM)
	}

	allowedEnv := newTaskTestEnv(t, func(opts *TaskToolOptions) {
		opts.AllowParallelInProgress = true
	})
	allowedEnv.start(t)
	result = allowedEnv.todo.Execute(allowedEnv.humanCtx(), map[string]any{"todos": parallel})
	if result.IsError {
		t.Fatalf("parallel in_progress should be allowed: %s", result.ForLLM)
	}
	if got := allowedEnv.requireState(t); len(got.Todos) != 2 {
		t.Fatalf("todos = %+v", got.Todos)
	}
}

func TestTodoWriteNoState(t *testing.T) {
	env := newTaskTestEnv(t, nil)
	result := env.todo.Execute(env.humanCtx(), map[string]any{
		"todos": []any{map[string]any{"content": "步骤", "status": "pending"}},
	})
	if !result.IsError || !strings.Contains(result.ForLLM, "请先调用 task_update start") {
		t.Fatalf("todo_write without state = %q", result.ForLLM)
	}

	// Paused and blocked tasks can still update the todo list.
	env.start(t)
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "pause"}); result.IsError {
		t.Fatalf("pause: %s", result.ForLLM)
	}
	if result := env.todo.Execute(env.humanCtx(), map[string]any{
		"todos": []any{map[string]any{"content": "暂停时更新", "status": "pending"}},
	}); result.IsError {
		t.Fatalf("todo_write on paused task: %s", result.ForLLM)
	}

	// Completed tasks reject todo changes.
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{"action": "resume"}); result.IsError {
		t.Fatalf("resume before complete: %s", result.ForLLM)
	}
	if result := env.execUpdate(t, env.humanCtx(), map[string]any{
		"action":   "complete",
		"evidence": []any{"证据"},
	}); result.IsError {
		t.Fatalf("complete: %s", result.ForLLM)
	}
	if result := env.todo.Execute(env.humanCtx(), map[string]any{
		"todos": []any{map[string]any{"content": "完成后更新", "status": "pending"}},
	}); !result.IsError {
		t.Fatal("todo_write on complete task should fail")
	}
}

func TestTaskToolSessionOwnership(t *testing.T) {
	env := newTaskTestEnv(t, nil)

	// No owning session in the tool context.
	noSession := context.Background()
	for name, result := range map[string]*ToolResult{
		"task_get":    env.get.Execute(noSession, nil),
		"task_update": env.update.Execute(noSession, map[string]any{"action": "pause"}),
		"todo_write":  env.todo.Execute(noSession, map[string]any{"todos": []any{}}),
	} {
		if !result.IsError || !strings.Contains(result.ForLLM, "需要所属的 agent session") {
			t.Fatalf("%s without session = %q", name, result.ForLLM)
		}
	}

	// Two sessions keep independent task state.
	otherKey := "agent:main:other"
	otherCtx := WithTurnSource(
		WithToolSessionContext(context.Background(), "main", otherKey, nil),
		SourceHuman,
	)
	if result := env.update.Execute(otherCtx, map[string]any{
		"action":    "start",
		"objective": "另一个会话的任务",
	}); result.IsError {
		t.Fatalf("start on other session: %s", result.ForLLM)
	}
	if _, ok := env.manager.GetTaskState(env.key); ok {
		t.Fatal("task state must stay scoped to the owning session")
	}
	if _, ok := env.manager.GetTaskState(otherKey); !ok {
		t.Fatal("other session should have its own task state")
	}

	// sessionKey cannot be overridden through tool arguments.
	if result := env.update.Execute(env.humanCtx(), map[string]any{
		"action":      "start",
		"objective":   "目标",
		"session_key": otherKey,
	}); result.IsError {
		t.Fatalf("start should ignore unknown session_key arg: %s", result.ForLLM)
	}
	if _, ok := env.manager.GetTaskState(env.key); !ok {
		t.Fatal("start should write to the ctx session, not an argument")
	}
}

// unsupportedSessionStore implements session.SessionStore but not
// taskstate.Store, to exercise the "store does not support task state" path.
type unsupportedSessionStore struct{}

func (unsupportedSessionStore) AddMessage(string, string, string)        {}
func (unsupportedSessionStore) AddFullMessage(string, providers.Message) {}
func (unsupportedSessionStore) GetHistory(string) []providers.Message    { return nil }
func (unsupportedSessionStore) GetSummary(string) string                 { return "" }
func (unsupportedSessionStore) SetSummary(string, string)                {}
func (unsupportedSessionStore) SetHistory(string, []providers.Message)   {}
func (unsupportedSessionStore) TruncateHistory(string, int)              {}
func (unsupportedSessionStore) Save(string) error                        { return nil }
func (unsupportedSessionStore) ListSessions() []string                   { return nil }
func (unsupportedSessionStore) Close() error                             { return nil }

func TestTaskStateStoreUnsupported(t *testing.T) {
	opts := TaskToolOptions{Sessions: unsupportedSessionStore{}}
	getTool := NewTaskGetTool(opts)
	updateTool := NewTaskUpdateTool(opts)
	todoTool := NewTodoWriteTool(opts)
	ctx := WithToolSessionContext(context.Background(), "main", "s1", nil)

	if result := getTool.Execute(ctx, nil); !result.IsError ||
		!strings.Contains(result.ForLLM, "不支持任务状态") {
		t.Fatalf("task_get unsupported = %q", result.ForLLM)
	}
	if result := updateTool.Execute(ctx, map[string]any{"action": "start", "objective": "目标"}); !result.IsError ||
		!strings.Contains(result.ForLLM, "不支持任务状态") {
		t.Fatalf("task_update unsupported = %q", result.ForLLM)
	}
	if result := todoTool.Execute(ctx, map[string]any{"todos": []any{}}); !result.IsError ||
		!strings.Contains(result.ForLLM, "不支持任务状态") {
		t.Fatalf("todo_write unsupported = %q", result.ForLLM)
	}

	// A nil store is also unsupported, not a panic.
	if result := NewTaskGetTool(TaskToolOptions{}).Execute(ctx, nil); !result.IsError {
		t.Fatal("nil store should produce an explicit error")
	}
}

func TestTaskToolDescriptionsAndSchemas(t *testing.T) {
	env := newTaskTestEnv(t, nil)

	if env.get.Name() != "task_get" || env.update.Name() != "task_update" || env.todo.Name() != "todo_write" {
		t.Fatalf("tool names = %q, %q, %q", env.get.Name(), env.update.Name(), env.todo.Name())
	}
	if !strings.Contains(env.get.Description(), "任务状态") ||
		!strings.Contains(env.update.Description(), "至少 3 轮") ||
		!strings.Contains(env.todo.Description(), "任务清单") {
		t.Fatalf("descriptions are not the Chinese prompt text: %q | %q | %q",
			env.get.Description(), env.update.Description(), env.todo.Description())
	}

	updateSchema := env.update.Parameters()
	required, _ := updateSchema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("task_update required = %v", required)
	}
	properties, _ := updateSchema["properties"].(map[string]any)
	action, _ := properties["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 5 {
		t.Fatalf("task_update action enum = %v", enum)
	}

	todoSchema := env.todo.Parameters()
	todoProperties, _ := todoSchema["properties"].(map[string]any)
	todos, _ := todoProperties["todos"].(map[string]any)
	items, _ := todos["items"].(map[string]any)
	itemProperties, _ := items["properties"].(map[string]any)
	status, _ := itemProperties["status"].(map[string]any)
	statusEnum, _ := status["enum"].([]string)
	if len(statusEnum) != 3 {
		t.Fatalf("todo_write status enum = %v", statusEnum)
	}
	if _, ok := todoSchema["required"]; !ok {
		t.Fatal("todo_write schema must require todos")
	}
}
