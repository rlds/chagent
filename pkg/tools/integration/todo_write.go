package integrationtools

import (
	"context"
	"fmt"

	"github.com/rlds/chagent/pkg/taskstate"
)

// TodoWriteTool 实现 `todo_write` 工具：替换整个会话的待办清单。
// 状态与并行进行中策略在执行时校验。
type TodoWriteTool struct {
	taskStateToolBase
}

// NewTodoWriteTool 创建 todo_write 工具。
func NewTodoWriteTool(opts TaskToolOptions) *TodoWriteTool {
	return &TodoWriteTool{taskStateToolBase: taskStateToolBase{opts: opts}}
}

// Name 实现 Tool。
func (t *TodoWriteTool) Name() string { return "todo_write" }

// Description 实现 Tool（Prompt P-12，中文）。
func (t *TodoWriteTool) Description() string {
	return "记录并更新任务清单，用于规划多步工作并展示进度；简单单步任务可跳过。" +
		"开始前为每个具体步骤添加一条待办。工作未完成时，保持正在处理的任务为 in_progress；" +
		"只有并行工作时才允许多个 in_progress。任务一完成就标记为 completed。" +
		"如果当前会话还没有任务状态，请先调用 task_update start。"
}

// Parameters 实现 Tool（Prompt P-12）。
func (t *TodoWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "完整任务清单，替换此前的清单。",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"content": map[string]any{
							"type":        "string",
							"description": "任务是什么，一句简短命令式描述。",
						},
						"status": map[string]any{
							"type":        "string",
							"description": "pending（未开始）| in_progress（进行中）| completed（已完成）。",
							"enum":        []string{taskstate.TodoPending, taskstate.TodoInProgress, taskstate.TodoCompleted},
						},
					},
					"required": []string{"content", "status"},
				},
			},
		},
		"required": []string{"todos"},
	}
}

// Execute 实现 Tool。
func (t *TodoWriteTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	sessionKey, store, errResult := t.resolve(ctx, t.Name())
	if errResult != nil {
		return errResult
	}

	state, found := loadState(store, sessionKey)
	if !found {
		return ErrorResult("错误：当前会话没有任务状态；请先调用 task_update start")
	}
	switch state.Phase {
	case taskstate.PhaseActive, taskstate.PhasePaused, taskstate.PhaseBlocked:
		// 允许
	case taskstate.PhaseComplete:
		return ErrorResult("错误：任务已完成，不能更新任务清单")
	default:
		return ErrorResult(fmt.Sprintf("错误：当前任务阶段 %s 不允许更新任务清单", state.Phase))
	}

	todos, err := getTodoArgs(args, t.opts.AllowParallelInProgress)
	if err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	state.Todos = todos
	state.MarkProgress(t.opts.now())
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}

	pending, inProgress, completed := 0, 0, 0
	for _, todo := range todos {
		switch todo.Status {
		case taskstate.TodoPending:
			pending++
		case taskstate.TodoInProgress:
			inProgress++
		case taskstate.TodoCompleted:
			completed++
		}
	}
	return NewToolResult(fmt.Sprintf(
		"任务清单已更新：%d 项待处理，%d 项进行中，%d 项已完成。",
		pending, inProgress, completed,
	))
}
