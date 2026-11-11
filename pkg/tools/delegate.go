package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/routing"
)

// DelegateTool 把一个任务委派给指定的具名 agent 并等待结果。
// 与 spawn（异步、发后即忘）或 subagent（同步但通用）不同，
// delegate 以具名 agent 为目标，并使用该 agent 自己的工作区、
// 模型和工具来运行任务。
type DelegateTool struct {
	spawner        SubTurnSpawner
	allowlistCheck func(targetAgentID string) bool
	selfAgentID    string
}

func NewDelegateTool() *DelegateTool {
	return &DelegateTool{}
}

func (t *DelegateTool) SetSpawner(spawner SubTurnSpawner) {
	t.spawner = spawner
}

func (t *DelegateTool) SetAllowlistChecker(check func(targetAgentID string) bool) {
	t.allowlistCheck = check
}

func (t *DelegateTool) SetSelfAgentID(id string) {
	t.selfAgentID = id
}

func (t *DelegateTool) Name() string {
	return "delegate"
}

func (t *DelegateTool) Description() string {
	return "把任务委派给另一个 agent 并等待结果。" +
		"当另一个 agent 根据自身能力更适合处理特定任务时使用。" +
		"目标 agent 使用自己的工作区、模型和工具独立运行。"
}

func (t *DelegateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent_id": map[string]any{
				"type":        "string",
				"description": "接收委派任务的目标 agent ID",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "要委派的任务的清晰描述",
			},
		},
		"required": []string{"agent_id", "task"},
	}
}

func (t *DelegateTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	rawAgentID, _ := args["agent_id"].(string)
	if strings.TrimSpace(rawAgentID) == "" {
		return ErrorResult("agent_id 必填且必须是非空字符串")
	}
	agentID := routing.NormalizeAgentID(rawAgentID)

	task, _ := args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return ErrorResult("task 必填且必须是非空字符串")
	}

	if t.selfAgentID != "" && agentID == t.selfAgentID {
		return ErrorResult("不能委托给自身")
	}

	if t.allowlistCheck != nil && !t.allowlistCheck(agentID) {
		return ErrorResult(fmt.Sprintf("不允许委托给 agent %q", agentID))
	}

	if t.spawner == nil {
		return ErrorResult("delegate 工具未配置")
	}

	result, err := t.spawner.SpawnSubTurn(ctx, SubTurnConfig{
		TargetAgentID: agentID,
		SystemPrompt:  task,
		Async:         false,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("委托给 agent %q 失败：%v", agentID, err)).WithError(err)
	}
	if result == nil {
		return ErrorResult(fmt.Sprintf("委托给 agent %q 未返回结果", agentID))
	}

	result.ForLLM = fmt.Sprintf("[来自 agent %q 的响应]\n%s", agentID, result.ForLLM)

	return result
}
