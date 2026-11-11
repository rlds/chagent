package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SpawnStatusTool 报告通过 spawn 工具派生的 subagent 的状态。
// 它可以按 ID 查询特定任务，也可以列出所有已知任务，
// 并给出按状态分类的汇总计数。
type SpawnStatusTool struct {
	manager *SubagentManager
}

// NewSpawnStatusTool 创建一个由给定 manager 支撑的 SpawnStatusTool。
func NewSpawnStatusTool(manager *SubagentManager) *SpawnStatusTool {
	return &SpawnStatusTool{manager: manager}
}

func (t *SpawnStatusTool) Name() string {
	return "spawn_status"
}

func (t *SpawnStatusTool) Description() string {
	return "获取已派生 subagent 的状态。返回所有 subagent 及其当前状态" +
		"（running、completed、failed 或 canceled）；提供 task_id 时" +
		"返回特定 subagent 任务的详情。结果按当前对话的 channel 和 chat ID 过滤；" +
		"只有在没有注入 channel/chat 上下文（例如通过 Execute 直接编程调用）时" +
		"才列出所有任务。"
}

func (t *SpawnStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type": "string",
				"description": "可选的任务 ID（例如 \"subagent-1\"），用于查看特定 subagent。" +
					"省略时列出所有可见 subagent。",
			},
		},
		"required": []string{},
	}
}

func (t *SpawnStatusTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	if t.manager == nil {
		return ErrorResult("未配置 subagent 管理器")
	}

	// 推导调用方对话的身份，以便把结果限定在当前会话内，
	// 避免多用户部署下跨对话的任务泄漏。
	callerChannel := ToolChannel(ctx)
	callerChatID := ToolChatID(ctx)

	var taskID string
	if rawTaskID, ok := args["task_id"]; ok && rawTaskID != nil {
		taskIDStr, ok := rawTaskID.(string)
		if !ok {
			return ErrorResult("task_id 必须是字符串")
		}
		taskID = strings.TrimSpace(taskIDStr)
	}

	if taskID != "" {
		// GetTaskCopy 在 manager 加锁状态下返回一致的快照，
		// 避免与并发运行的 subagent goroutine 产生数据竞争。
		taskCopy, ok := t.manager.GetTaskCopy(taskID)
		if !ok {
			return ErrorResult(fmt.Sprintf("未找到任务 ID 为 %s 的 subagent", taskID))
		}

		// 把查找范围限制为属于当前对话的任务。
		if callerChannel != "" && taskCopy.OriginChannel != "" && taskCopy.OriginChannel != callerChannel {
			return ErrorResult(fmt.Sprintf("未找到任务 ID 为 %s 的 subagent", taskID))
		}
		if callerChatID != "" && taskCopy.OriginChatID != "" && taskCopy.OriginChatID != callerChatID {
			return ErrorResult(fmt.Sprintf("未找到任务 ID 为 %s 的 subagent", taskID))
		}

		return NewToolResult(spawnStatusFormatTask(&taskCopy))
	}

	// ListTaskCopies 在 manager 加锁状态下返回一致的快照。
	origTasks := t.manager.ListTaskCopies()
	if len(origTasks) == 0 {
		return NewToolResult("尚未派生任何 subagent。")
	}

	tasks := make([]*SubagentTask, 0, len(origTasks))
	for i := range origTasks {
		cpy := &origTasks[i]

		// 只保留源自当前对话的任务。
		if callerChannel != "" && cpy.OriginChannel != "" && cpy.OriginChannel != callerChannel {
			continue
		}
		if callerChatID != "" && cpy.OriginChatID != "" && cpy.OriginChatID != callerChatID {
			continue
		}

		tasks = append(tasks, cpy)
	}

	if len(tasks) == 0 {
		return NewToolResult("当前对话中没有 subagent。")
	}

	// 按创建时间（升序）排序，以保持派生顺序。
	// 对于同一毫秒内创建的任务，改用 ID 字符串排序。
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Created != tasks[j].Created {
			return tasks[i].Created < tasks[j].Created
		}
		return tasks[i].ID < tasks[j].ID
	})

	counts := map[string]int{}
	for _, task := range tasks {
		counts[task.Status]++
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("subagent 状态报告（共 %d 个）：\n", len(tasks)))
	for _, status := range []string{"running", "completed", "failed", "canceled"} {
		if n := counts[status]; n > 0 {
			label := subagentStatusLabel(status)
			sb.WriteString(fmt.Sprintf("  %s: %d\n", label, n))
		}
	}
	sb.WriteString("\n")

	for _, task := range tasks {
		sb.WriteString(spawnStatusFormatTask(task))
		sb.WriteString("\n\n")
	}

	return NewToolResult(strings.TrimRight(sb.String(), "\n"))
}

// spawnStatusFormatTask 把单个 SubagentTask 渲染成人类可读的文本块。
func spawnStatusFormatTask(task *SubagentTask) string {
	var sb strings.Builder

	header := fmt.Sprintf("[%s] 状态=%s", task.ID, task.Status)
	if task.Label != "" {
		header += fmt.Sprintf("  标签=%q", task.Label)
	}
	if task.AgentID != "" {
		header += fmt.Sprintf("  agent=%s", task.AgentID)
	}
	if task.Created > 0 {
		created := time.UnixMilli(task.Created).UTC().Format("2006-01-02 15:04:05 UTC")
		header += fmt.Sprintf("  创建时间=%s", created)
	}
	sb.WriteString(header)

	if task.Task != "" {
		sb.WriteString(fmt.Sprintf("\n  任务：%s", task.Task))
	}
	if task.Result != "" {
		result := task.Result
		const maxResultLen = 300
		runes := []rune(result)
		if len(runes) > maxResultLen {
			result = string(runes[:maxResultLen]) + "…"
		}
		sb.WriteString(fmt.Sprintf("\n  结果：%s", result))
	}

	return sb.String()
}

// subagentStatusLabel 把内部状态标识映射为模型可读中文标签；
// 状态值本身（running/completed/...）作为技术标识保留在每任务块的 `status=` 中。
func subagentStatusLabel(status string) string {
	switch status {
	case "running":
		return "运行中"
	case "completed":
		return "已完成"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	default:
		return status
	}
}
