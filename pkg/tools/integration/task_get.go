package integrationtools

import (
	"context"
	"encoding/json"

	"github.com/rlds/chagent/pkg/taskstate"
)

// TaskGetTool 实现 `task_get` 工具：把当前会话的任务状态读取为
// 紧凑、稳定的 JSON。它没有副作用。
type TaskGetTool struct {
	taskStateToolBase
}

// NewTaskGetTool 创建一个 task_get 工具。
func NewTaskGetTool(opts TaskToolOptions) *TaskGetTool {
	return &TaskGetTool{taskStateToolBase: taskStateToolBase{opts: opts}}
}

// Name 实现 Tool。
func (t *TaskGetTool) Name() string { return "task_get" }

// Description 实现 Tool（Prompt P-13，中文）。
func (t *TaskGetTool) Description() string {
	return "读取当前会话的任务状态：目标、阶段、轮次、待办、证据和阻塞原因。更新状态前或不确定还剩什么工作时使用。"
}

// Parameters 实现 Tool（Prompt P-13）。
func (t *TaskGetTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// taskGetPayload 是稳定的 task_get JSON 结构。
type taskGetPayload struct {
	Task *taskGetTask `json:"task"`
}

type taskGetTask struct {
	Objective     string                   `json:"objective"`
	Phase         string                   `json:"phase"`
	Round         int                      `json:"round"`
	MaxRounds     int                      `json:"max_rounds"`
	Todos         []taskstate.Todo         `json:"todos"`
	Evidence      []string                 `json:"evidence"`
	BlockedReason *taskstate.BlockedReason `json:"blocked_reason"`
}

// Execute 实现 Tool。
func (t *TaskGetTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	sessionKey, store, errResult := t.resolve(ctx, t.Name())
	if errResult != nil {
		return errResult
	}

	state, found := loadState(store, sessionKey)
	payload := taskGetPayload{}
	if found {
		todos := state.Todos
		if todos == nil {
			todos = []taskstate.Todo{}
		}
		evidence := state.Evidence
		if evidence == nil {
			evidence = []string{}
		}
		var blockedReason *taskstate.BlockedReason
		if state.BlockedReason != nil {
			reason := *state.BlockedReason
			blockedReason = &reason
		}
		payload.Task = &taskGetTask{
			Objective:     state.Objective,
			Phase:         string(state.Phase),
			Round:         state.Round,
			MaxRounds:     state.MaxRounds,
			Todos:         todos,
			Evidence:      evidence,
			BlockedReason: blockedReason,
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return ErrorResult("错误：无法编码任务状态：" + err.Error())
	}
	return NewToolResult(string(data))
}
