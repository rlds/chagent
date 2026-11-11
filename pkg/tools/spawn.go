package tools

import (
	"context"
	"fmt"
	"strings"
)

type SpawnTool struct {
	spawner        SubTurnSpawner
	manager        *SubagentManager
	defaultModel   string
	maxTokens      int
	temperature    float64
	allowlistCheck func(targetAgentID string) bool
}

// 编译期检查：SpawnTool 实现了 AsyncExecutor。
var _ AsyncExecutor = (*SpawnTool)(nil)

func NewSpawnTool(manager *SubagentManager) *SpawnTool {
	if manager == nil {
		return &SpawnTool{}
	}
	return &SpawnTool{
		defaultModel: manager.defaultModel,
		maxTokens:    manager.maxTokens,
		temperature:  manager.temperature,
		manager:      manager,
	}
}

// SetSpawner 设置用于直接执行子回合的 SubTurnSpawner。
func (t *SpawnTool) SetSpawner(spawner SubTurnSpawner) {
	t.spawner = spawner
}

func (t *SpawnTool) Name() string {
	return "spawn"
}

func (t *SpawnTool) Description() string {
	return "派生一个后台 subagent 处理任务。适用于可独立运行的复杂或耗时任务。subagent 完成后会返回报告。"
}

func (t *SpawnTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "subagent 要完成的任务",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "可选的短标签（用于展示）",
			},
			"agent_id": map[string]any{
				"type":        "string",
				"description": "可选的目标 agent ID，用于把任务委派给它",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SpawnTool) SetAllowlistChecker(check func(targetAgentID string) bool) {
	t.allowlistCheck = check
}

func (t *SpawnTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.execute(ctx, args, nil)
}

// ExecuteAsync 实现 AsyncExecutor。回调作为调用参数透传给
// subagent 管理器 —— 绝不会存放在 SpawnTool 实例上。
func (t *SpawnTool) ExecuteAsync(
	ctx context.Context,
	args map[string]any,
	cb AsyncCallback,
) *ToolResult {
	return t.execute(ctx, args, cb)
}

func (t *SpawnTool) execute(
	ctx context.Context,
	args map[string]any,
	cb AsyncCallback,
) *ToolResult {
	task, ok := args["task"].(string)
	if !ok || strings.TrimSpace(task) == "" {
		return ErrorResult("缺少必填参数 task，且必须为非空字符串")
	}

	label, ok := args["label"].(string)
	if !ok {
		label = ""
	}
	agentID, ok := args["agent_id"].(string)
	if !ok {
		agentID = ""
	}
	targetAgentID := strings.TrimSpace(agentID)

	// 若目标是特定 agent，则检查允许列表（allowlist）
	if targetAgentID != "" && t.allowlistCheck != nil {
		if !t.allowlistCheck(targetAgentID) {
			return ErrorResult(fmt.Sprintf("不允许派生 agent「%s」", targetAgentID))
		}
	}

	// 为派生的 subagent 构建系统提示词
	systemPrompt := SubagentSystemPrompt(label, task, true)

	// 有可用 spawner 时直接使用（直接调用 SpawnSubTurn）
	if t.spawner != nil {
		// 把任务注册到共享的 SubagentManager，使 DSH 控制面工具
		// （list_agents/wait_agent/interrupt_agent）能够看到并取消它。
		// Ctx 可取消：interrupt_agent 会取消它，goroutine 通过 spawner
		// 感知到取消。
		taskCtx := ctx
		var tracked *TrackedSubagentTask
		if t.manager != nil {
			if handle, trackErr := t.manager.TrackSpawn(
				ctx,
				task,
				label,
				targetAgentID,
				ToolChannel(ctx),
				ToolChatID(ctx),
			); trackErr == nil && handle != nil {
				tracked = handle
				taskCtx = handle.Ctx
			}
		}

		// 在 goroutine 中启动异步子回合
		go func() {
			result, err := t.spawner.SpawnSubTurn(taskCtx, SubTurnConfig{
				Model:         t.defaultModel,
				Tools:         nil, // 将通过上下文从父级继承
				SystemPrompt:  systemPrompt,
				MaxTokens:     t.maxTokens,
				Temperature:   t.temperature,
				Async:         true, // 异步执行
				Critical:      true, // 后台派生应能在父回合结束后继续存活
				TargetAgentID: targetAgentID,
			})
			if err != nil {
				result = ErrorResult(fmt.Sprintf("派生 subagent 失败：%v", err)).WithError(err)
			}

			// 在回调之前把终态发布到控制面，使 wait_agent/interrupt_agent
			// 观察到真实结果。
			if tracked != nil {
				status := SubagentStatusCompleted
				summary := ""
				if result != nil {
					summary = result.ForLLM
					if result.IsError {
						status = SubagentStatusFailed
					}
				}
				if taskCtx.Err() != nil {
					status = SubagentStatusCanceled
					if summary == "" {
						summary = "任务已取消：" + taskCtx.Err().Error()
					}
				}
				tracked.Finish(status, summary)
			}

			// 如果提供了回调则调用
			if cb != nil {
				cb(ctx, result)
			}
		}()

		// 立即返回确认。任务被跟踪时，带上其 ID，
		// 以便模型调用 list_agents/wait_agent/interrupt_agent。
		var ack string
		if label != "" {
			ack = fmt.Sprintf("已派生 subagent「%s」处理任务：%s", label, task)
		} else {
			ack = fmt.Sprintf("已派生 subagent 处理任务：%s", task)
		}
		if tracked != nil {
			ack += fmt.Sprintf("（任务 ID：%s；可使用 list_agents/wait_agent/interrupt_agent 跟踪）", tracked.ID)
		}
		return AsyncResult(ack)
	}

	// 回退：未配置 spawner
	return ErrorResult("未配置 subagent 管理器")
}
