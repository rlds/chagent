// Package subagentctl 实现 DSH subagent 控制面工具组：
// list_agents、wait_agent 和 interrupt_agent。
//
// 这些工具只暴露共享 tools.SubagentManager 的真实能力：
//
//   - list_agents     读取调用会话可见任务的一致快照（按 channel/chat 限定作用域，
//     规则与 spawn_status 相同）；
//   - wait_agent      轮询 manager，直到某个任务结束或调用方的超时到期；
//   - interrupt_agent 取消运行中任务的 task context。任务没有取消句柄时，
//     它会返回明确的 "not interruptible" 错误，而不是假装任务已停止。
package subagentctl

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// 工具名和提示词元数据，按 DSH 工具注册表的要求提供。
const (
	ListAgentsToolName     = "list_agents"
	WaitAgentToolName      = "wait_agent"
	InterruptAgentToolName = "interrupt_agent"

	PromptLayer  = "capability"
	PromptSlot   = "dsh"
	PromptSource = "tool_registry:dsh"
)

const (
	defaultListLimit = 20
	maxListLimit     = 50

	defaultWaitTimeoutMs = 30_000
	maxWaitTimeoutMs     = 300_000
	waitPollInterval     = 100 * time.Millisecond

	defaultInterruptWaitMs = 2_000
	maxInterruptWaitMs     = 10_000
	interruptPollInterval  = 50 * time.Millisecond

	// maxResultRunes 限制返回给模型的每个结果 / 任务摘要的长度。
	maxResultRunes = 400
)

// TaskManager 是控制面用到的那部分 *tools.SubagentManager 能力。把它保留为
// 接口可以让工具借助假实现做单元测试，而生产环境接线时传入真实的 manager。
type TaskManager interface {
	ListTaskCopies() []tools.SubagentTask
	GetTaskCopy(taskID string) (tools.SubagentTask, bool)
	CancelTask(taskID string) (tools.SubagentTask, error)
	WaitTask(ctx context.Context, taskID string, poll time.Duration) (tools.SubagentTask, error)
}

var (
	// ErrManagerNotConfigured 在未配置 SubagentManager 却使用控制面时返回
	//（例如 subagent 运行时已关闭）。
	ErrManagerNotConfigured = errors.New("subagent manager not configured")
)

// Register 把三个控制面工具添加到给定的 registry。
func Register(r *tools.ToolRegistry, mgr *tools.SubagentManager) {
	if r == nil {
		return
	}
	r.Register(NewListAgentsTool(mgr))
	r.Register(NewWaitAgentTool(mgr))
	r.Register(NewInterruptAgentTool(mgr))
}

// metadataForMaintenance 把维护 / 控制类工具标记为提示词可发现。
func metadataForMaintenance() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  PromptLayer,
		Slot:   PromptSlot,
		Source: PromptSource,
	}
}

// visibleTask 报告任务是否属于调用方的会话。规则与 spawn_status 一致：空的
// 调用方上下文（直接的编程式调用）能看到全部内容；否则，在任务记录了 channel
// 和 chat ID 时，两者都必须匹配。
func visibleTask(ctx context.Context, task tools.SubagentTask) bool {
	callerChannel := tools.ToolChannel(ctx)
	callerChatID := tools.ToolChatID(ctx)
	if callerChannel != "" && task.OriginChannel != "" && task.OriginChannel != callerChannel {
		return false
	}
	if callerChatID != "" && task.OriginChatID != "" && task.OriginChatID != callerChatID {
		return false
	}
	return true
}

func normalizeStatusFilter(raw string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(raw))
	if status == "" {
		return "", nil
	}
	switch status {
	case tools.SubagentStatusRunning, tools.SubagentStatusCompleted,
		tools.SubagentStatusFailed, tools.SubagentStatusCanceled:
		return status, nil
	default:
		return "", fmt.Errorf(
			"status=%q 无效：可选值为 running/completed/failed/canceled",
			raw,
		)
	}
}

func truncateRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		limit = maxResultRunes
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func taskSummaryLine(task tools.SubagentTask) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("- id=%s status=%s", task.ID, task.Status))
	if task.Label != "" {
		sb.WriteString(fmt.Sprintf(" label=%q", task.Label))
	}
	if task.AgentID != "" {
		sb.WriteString(fmt.Sprintf(" agent=%s", task.AgentID))
	}
	if task.Created > 0 {
		sb.WriteString(" 创建时间=" + time.UnixMilli(task.Created).UTC().Format("2006-01-02T15:04:05Z"))
	}
	if task.Task != "" {
		sb.WriteString("\n  任务：" + truncateRunes(task.Task, 200))
	}
	if task.Result != "" {
		sb.WriteString("\n  结果：" + truncateRunes(task.Result, maxResultRunes))
	}
	return sb.String()
}

func managerError(toolName string, err error) *tools.ToolResult {
	return tools.ErrorResult(fmt.Sprintf("%s 失败：%v", toolName, err)).WithError(err)
}

// ---------------------------------------------------------------------------
// list_agents
// ---------------------------------------------------------------------------

// ListAgentsTool 列出调用会话可见的 subagent 任务。
type ListAgentsTool struct {
	manager TaskManager
}

// NewListAgentsTool 创建一个由给定 manager 支撑的 list_agents 工具。
func NewListAgentsTool(manager TaskManager) *ListAgentsTool {
	return &ListAgentsTool{manager: manager}
}

func (t *ListAgentsTool) Name() string { return ListAgentsToolName }

func (t *ListAgentsTool) Description() string {
	return "列出当前会话可见的 subagent 任务（id、状态、结果摘要、创建时间，可含任务/标签/目标 agent），" +
		"用于查看 spawn 派生的后台任务。结果按 channel/chat 隔离，仅包含本会话任务；" +
		"可用 status 过滤，limit 控制条数（默认 20，最大 50）。"
}

func (t *ListAgentsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type":        "string",
				"description": "可选状态过滤",
				"enum":        []string{"running", "completed", "failed", "canceled"},
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "最多返回多少条（默认 20，最大 50）",
			},
		},
		"required": []string{},
	}
}

func (t *ListAgentsTool) PromptMetadata() toolshared.PromptMetadata {
	return metadataForMaintenance()
}

// ConcurrencySafe 为 true：list_agents 只读取快照。
func (t *ListAgentsTool) ConcurrencySafe() bool { return true }

func (t *ListAgentsTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	if t.manager == nil {
		return tools.ErrorResult(
			"list_agents 不可用：subagent 控制面未配置（需要启用 subagent/spawn 工具）。",
		).WithError(ErrManagerNotConfigured)
	}

	statusFilter := ""
	if raw, ok := args["status"]; ok && raw != nil {
		value, isString := raw.(string)
		if !isString {
			return tools.ErrorResult("status 必须是字符串")
		}
		normalized, err := normalizeStatusFilter(value)
		if err != nil {
			return tools.ErrorResult(err.Error()).WithError(err)
		}
		statusFilter = normalized
	}

	limit := defaultListLimit
	if raw, ok := args["limit"]; ok && raw != nil {
		value, isNumber := asInt(raw)
		if !isNumber {
			return tools.ErrorResult("limit 必须是整数")
		}
		limit = value
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	tasks := make([]tools.SubagentTask, 0, limit)
	for _, task := range t.manager.ListTaskCopies() {
		if !visibleTask(ctx, task) {
			continue
		}
		if statusFilter != "" && task.Status != statusFilter {
			continue
		}
		tasks = append(tasks, task)
	}

	if len(tasks) == 0 {
		if statusFilter != "" {
			return tools.NewToolResult(fmt.Sprintf(
				"当前会话没有 status=%s 的 subagent 任务。", statusFilter))
		}
		return tools.NewToolResult("当前会话没有可用的 subagent 任务。")
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Created != tasks[j].Created {
			return tasks[i].Created < tasks[j].Created
		}
		return tasks[i].ID < tasks[j].ID
	})

	total := len(tasks)
	truncated := false
	if len(tasks) > limit {
		tasks = tasks[:limit]
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("subagent 任务列表（共 %d 条", total))
	if truncated {
		sb.WriteString(fmt.Sprintf("，已截断为前 %d 条", len(tasks)))
	}
	sb.WriteString("）：\n")
	for _, task := range tasks {
		sb.WriteString(taskSummaryLine(task))
		sb.WriteString("\n")
	}
	return tools.NewToolResult(strings.TrimRight(sb.String(), "\n"))
}

// ---------------------------------------------------------------------------
// wait_agent
// ---------------------------------------------------------------------------

// WaitAgentTool 等待某个 subagent 任务结束并返回其摘要。
type WaitAgentTool struct {
	manager TaskManager
}

// NewWaitAgentTool 创建一个由给定 manager 支撑的 wait_agent 工具。
func NewWaitAgentTool(manager TaskManager) *WaitAgentTool {
	return &WaitAgentTool{manager: manager}
}

func (t *WaitAgentTool) Name() string { return WaitAgentToolName }

func (t *WaitAgentTool) Description() string {
	return "等待指定 subagent 任务结束并返回最终状态与结果摘要。任务已结束时立即返回；" +
		"超时返回明确错误（任务仍在运行，不会谎报完成）。仅能等待当前会话可见的任务。"
}

func (t *WaitAgentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "要等待的 subagent 任务 ID（例如 subagent-1）",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "最长等待毫秒数（默认 30000，最大 300000）",
			},
		},
		"required": []string{"task_id"},
	}
}

func (t *WaitAgentTool) PromptMetadata() toolshared.PromptMetadata {
	return metadataForMaintenance()
}

// ConcurrencySafe 为 true：wait_agent 只观察 manager 状态。
func (t *WaitAgentTool) ConcurrencySafe() bool { return true }

func (t *WaitAgentTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	if t.manager == nil {
		return tools.ErrorResult(
			"wait_agent 不可用：subagent 控制面未配置（需要启用 subagent/spawn 工具）。",
		).WithError(ErrManagerNotConfigured)
	}

	taskID, err := requiredTaskID(args)
	if err != nil {
		return tools.ErrorResult(err.Error()).WithError(err)
	}

	timeout := time.Duration(defaultWaitTimeoutMs) * time.Millisecond
	if raw, ok := args["timeout_ms"]; ok && raw != nil {
		value, isNumber := asInt(raw)
		if !isNumber {
			return tools.ErrorResult("timeout_ms 必须是整数")
		}
		timeout = time.Duration(value) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = time.Duration(defaultWaitTimeoutMs) * time.Millisecond
	}
	if timeout > time.Duration(maxWaitTimeoutMs)*time.Millisecond {
		timeout = time.Duration(maxWaitTimeoutMs) * time.Millisecond
	}

	task, ok := t.manager.GetTaskCopy(taskID)
	if !ok || !visibleTask(ctx, task) {
		return tools.ErrorResult(fmt.Sprintf(
			"找不到 subagent 任务 %q（可能属于其他会话、ID 错误或已被清理）。", taskID,
		)).WithError(tools.ErrSubagentTaskNotFound)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	settled, waitErr := t.manager.WaitTask(waitCtx, taskID, waitPollInterval)
	if waitErr != nil {
		if errors.Is(waitErr, tools.ErrSubagentTaskNotFound) {
			return tools.ErrorResult(fmt.Sprintf("找不到 subagent 任务 %q。", taskID)).WithError(waitErr)
		}
		// 超时：报告真实的当前状态，绝不能谎称任务已完成。
		current, ok := t.manager.GetTaskCopy(taskID)
		if !ok {
			return tools.ErrorResult(fmt.Sprintf("找不到 subagent 任务 %q（等待期间被清理）。", taskID)).WithError(waitErr)
		}
		return tools.ErrorResult(fmt.Sprintf(
			"等待 subagent %q 超时（%dms）：任务仍为 status=%s，未完成。可稍后再次 wait_agent，"+
				"或用 list_agents 查看进度；若任务确实无法完成，可用 interrupt_agent 请求中断。",
			taskID, timeout.Milliseconds(), current.Status,
		)).WithError(waitErr)
	}
	if !visibleTask(ctx, settled) {
		return tools.ErrorResult(fmt.Sprintf("找不到 subagent 任务 %q。", taskID))
	}

	headline := fmt.Sprintf("subagent %s 已结束：status=%s", settled.ID, settled.Status)
	if settled.Label != "" {
		headline += fmt.Sprintf("，label=%q", settled.Label)
	}
	if settled.Result != "" {
		headline += "\n结果摘要：" + truncateRunes(settled.Result, maxResultRunes)
	}
	return tools.NewToolResult(headline)
}

// ---------------------------------------------------------------------------
// interrupt_agent
// ---------------------------------------------------------------------------

// InterruptAgentTool 在执行层暴露取消句柄时取消运行中的 subagent 任务；
// 否则它会报告明确的不可中断错误。
type InterruptAgentTool struct {
	manager TaskManager
}

// NewInterruptAgentTool 创建一个由 manager 支撑的 interrupt_agent 工具。
func NewInterruptAgentTool(manager TaskManager) *InterruptAgentTool {
	return &InterruptAgentTool{manager: manager}
}

func (t *InterruptAgentTool) Name() string { return InterruptAgentToolName }

func (t *InterruptAgentTool) Description() string {
	return "请求中断正在运行的 subagent 任务（取消其执行上下文）。仅对当前会话可见、" +
		"处于 running 且执行器暴露取消能力的任务生效；已结束的任务会说明无需中断，" +
		"不可中断时会明确报错（不会谎报已中断）。可用 wait_ms 等待取消生效。"
}

func (t *InterruptAgentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "要中断的 subagent 任务 ID（例如 subagent-1）",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "发送取消后等待任务终止的毫秒数（默认 2000，最大 10000）",
			},
		},
		"required": []string{"task_id"},
	}
}

func (t *InterruptAgentTool) PromptMetadata() toolshared.PromptMetadata {
	return metadataForMaintenance()
}

// ConcurrencySafe 为 false：interrupt_agent 会修改任务状态。
func (t *InterruptAgentTool) ConcurrencySafe() bool { return false }

func (t *InterruptAgentTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	if t.manager == nil {
		return tools.ErrorResult(
			"interrupt_agent 不可用：subagent 控制面未配置（需要启用 subagent/spawn 工具）。",
		).WithError(ErrManagerNotConfigured)
	}

	taskID, err := requiredTaskID(args)
	if err != nil {
		return tools.ErrorResult(err.Error()).WithError(err)
	}

	waitMs := int64(defaultInterruptWaitMs)
	if raw, ok := args["wait_ms"]; ok && raw != nil {
		value, isNumber := asInt(raw)
		if !isNumber {
			return tools.ErrorResult("wait_ms 必须是整数")
		}
		waitMs = int64(value)
	}
	if waitMs <= 0 {
		waitMs = defaultInterruptWaitMs
	}
	if waitMs > maxInterruptWaitMs {
		waitMs = maxInterruptWaitMs
	}

	task, ok := t.manager.GetTaskCopy(taskID)
	if !ok || !visibleTask(ctx, task) {
		return tools.ErrorResult(fmt.Sprintf(
			"找不到 subagent 任务 %q（可能属于其他会话、ID 错误或已被清理）。", taskID,
		)).WithError(tools.ErrSubagentTaskNotFound)
	}
	if task.Status != tools.SubagentStatusRunning {
		return tools.NewToolResult(fmt.Sprintf(
			"subagent %q 已经结束（status=%s），无需中断。结果摘要：%s",
			taskID, task.Status, truncateRunes(task.Result, maxResultRunes),
		))
	}

	snapshot, cancelErr := t.manager.CancelTask(taskID)
	if cancelErr != nil {
		switch {
		case errors.Is(cancelErr, tools.ErrSubagentTaskNotRunning):
			return tools.NewToolResult(fmt.Sprintf(
				"subagent %q 已经结束（status=%s），无需中断。",
				taskID, snapshot.Status,
			))
		case errors.Is(cancelErr, tools.ErrSubagentTaskNotInterruptible):
			return tools.ErrorResult(fmt.Sprintf(
				"不支持中断当前任务：subagent %q 的执行器没有暴露取消句柄（status=running）。"+
					"该任务会继续运行直到完成；请改用 wait_agent 等待结果。", taskID,
			)).WithError(cancelErr)
		case errors.Is(cancelErr, tools.ErrSubagentTaskNotFound):
			return tools.ErrorResult(fmt.Sprintf("找不到 subagent 任务 %q。", taskID)).WithError(cancelErr)
		default:
			return managerError(InterruptAgentToolName, cancelErr)
		}
	}

	// 取消请求确实已经发出。短暂轮询等待终态；
	// 若执行器未及时响应，就如实报告这一点。
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	for {
		current, found := t.manager.GetTaskCopy(taskID)
		if !found {
			return tools.NewToolResult(fmt.Sprintf(
				"已向 subagent %q 发送取消请求；任务随后被清理，无法确认最终状态。", taskID))
		}
		if current.Status != tools.SubagentStatusRunning {
			switch current.Status {
			case tools.SubagentStatusCanceled:
				return tools.NewToolResult(fmt.Sprintf(
					"已中断 subagent %q（status=canceled）。结果摘要：%s",
					taskID, truncateRunes(current.Result, maxResultRunes),
				))
			case tools.SubagentStatusFailed:
				return tools.NewToolResult(fmt.Sprintf(
					"已中断 subagent %q：取消请求生效，任务以 status=failed 结束。结果摘要：%s",
					taskID, truncateRunes(current.Result, maxResultRunes),
				))
			default:
				return tools.NewToolResult(fmt.Sprintf(
					"已向 subagent %q 发送取消请求，但任务在取消生效前已正常结束（status=%s）。",
					taskID, current.Status,
				))
			}
		}
		if time.Now().After(deadline) {
			return tools.NewToolResult(fmt.Sprintf(
				"已向 subagent %q 发送取消请求，但执行器在 %dms 内未报告终止；当前仍为 status=running"+
					"（可能处于不可中断的调用中）。这不代表已中断，请稍后用 list_agents/wait_agent 复核。",
				taskID, waitMs,
			))
		}
		select {
		case <-ctx.Done():
			return tools.NewToolResult(fmt.Sprintf(
				"取消请求已发送给 subagent %q，但等待期间调用方上下文结束，未能确认最终状态。", taskID))
		case <-time.After(interruptPollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// asInt 接受此处可能出现的各种数值形态：JSON 解码产生的 float64，
// 以及直接编程调用方 / 测试传入的 int/int64。
func asInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case float32:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case int32:
		return int(typed), true
	default:
		return 0, false
	}
}

func requiredTaskID(args map[string]any) (string, error) {
	raw, ok := args["task_id"]
	if !ok || raw == nil {
		return "", fmt.Errorf("task_id 必填")
	}
	value, isString := raw.(string)
	if !isString {
		return "", fmt.Errorf("task_id 必须是字符串")
	}
	taskID := strings.TrimSpace(value)
	if taskID == "" {
		return "", fmt.Errorf("task_id 不能为空")
	}
	return taskID, nil
}
