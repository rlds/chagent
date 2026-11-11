package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/constants"
	"github.com/rlds/chagent/pkg/cron"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/utils"
)

// JobExecutor 是借助 agent 执行定时任务的接口
type JobExecutor interface {
	ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string) (string, error)
	// PublishResponseIfNeeded 仅在 agent 本轮尚未通过 message 工具投递内容时，
	// 才把响应发送到出站总线。
	PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string, modelName string, usage *providers.UsageInfo)
}

// CronTool 为 agent 提供定时调度能力
type CronTool struct {
	cronService           *cron.CronService
	executor              JobExecutor
	msgBus                *bus.MessageBus
	execTool              *ExecTool
	allowCommand          bool
	execEnabled           bool
	commandAllowedRemotes []string
}

// NewCronTool 创建一个新的 CronTool
// execTimeout：0 表示不设超时，>0 表示设置超时时间
func NewCronTool(
	cronService *cron.CronService, executor JobExecutor, msgBus *bus.MessageBus, workspace string, restrict bool,
	execTimeout time.Duration, config *config.Config,
) (*CronTool, error) {
	allowCommand := true
	execEnabled := true
	var commandAllowedRemotes []string
	if config != nil {
		allowCommand = config.Tools.Cron.AllowCommand
		execEnabled = config.Tools.Exec.Enabled
		commandAllowedRemotes = config.Tools.Cron.CommandAllowedRemotes
	}

	var execTool *ExecTool
	if execEnabled {
		var err error
		execTool, err = NewExecToolWithConfig(workspace, restrict, config)
		if err != nil {
			return nil, fmt.Errorf("unable to configure exec tool: %w", err)
		}
	}

	if execTool != nil {
		execTool.SetTimeout(execTimeout)
	}
	return &CronTool{
		cronService:           cronService,
		executor:              executor,
		msgBus:                msgBus,
		execTool:              execTool,
		allowCommand:          allowCommand,
		execEnabled:           execEnabled,
		commandAllowedRemotes: commandAllowedRemotes,
	}, nil
}

// Name 返回工具名称
func (t *CronTool) Name() string {
	return "cron"
}

// Description 返回工具描述
func (t *CronTool) Description() string {
	return `创建、查看和更新提醒、任务或 shell 命令定时任务。当用户要求提醒或安排定时事项时，调用本工具。` +
		`一次性提醒用 at_seconds，重复间隔用 every_seconds（仅用于周期性任务），复杂周期用 cron_expr，` +
		`需要在触发时执行 shell 命令用 command。修改已有任务前先 action=get，避免丢失原载荷。`
}

// Parameters 返回工具参数 schema
//
//nolint:dupl // 工具参数 schema 有意使用相似的 JSON-schema map 字面量。
func (t *CronTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "list", "get", "update", "remove", "enable", "disable"},
				"description": "要执行的动作。远程通道只能对当前 channel/chat_id 的任务执行 list/get/update。",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "可选：任务显示名称，用于 add 或 update。",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "触发时展示的提醒/任务消息。若同时使用 command，此字段描述命令的作用。",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "可选：直接执行的 shell 命令（例如 'df -h'）。设置后 agent 会执行该命令并报告输出，而不是只展示消息。update 时省略表示保留原命令，传空字符串表示清除。",
			},
			"command_confirm": map[string]any{
				"type":        "boolean",
				"description": "可选：调度 shell 命令的显式确认标记。命令执行还必须在 tools.cron.allow_command 中启用。",
			},
			"at_seconds": map[string]any{
				"type":        "integer",
				"description": "一次性提醒：多少秒后触发（例如 600 表示 10 分钟后）。",
			},
			"every_seconds": map[string]any{
				"type":        "integer",
				"description": "重复间隔秒数（例如 3600 表示每小时）。",
			},
			"cron_expr": map[string]any{
				"type":        "string",
				"description": "复杂重复调度的 cron 表达式（例如 '0 9 * * *' 表示每天 9 点）。",
			},
			"job_id": map[string]any{
				"type":        "string",
				"description": "任务 ID（用于 get/update/remove/enable/disable）",
			},
		},
		"required": []string{"action"},
	}
}

// Execute 使用给定参数运行工具
func (t *CronTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, ok := args["action"].(string)
	if !ok {
		return ErrorResult("action 参数必填")
	}

	switch action {
	case "add":
		return t.addJob(ctx, args)
	case "list":
		return t.listJobs(ctx)
	case "get":
		return t.getJob(ctx, args)
	case "update":
		return t.updateJob(ctx, args)
	case "remove":
		return t.removeJob(ctx, args)
	case "enable":
		return t.enableJob(ctx, args, true)
	case "disable":
		return t.enableJob(ctx, args, false)
	default:
		return ErrorResult(fmt.Sprintf("未知 action：%s", action))
	}
}

func (t *CronTool) addJob(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)

	if channel == "" || chatID == "" {
		return ErrorResult("没有会话上下文（channel/chat_id 未设置）。请在活动会话中使用此工具。")
	}

	message, ok := args["message"].(string)
	if !ok || message == "" {
		return ErrorResult("add 操作需要 message 参数")
	}

	var schedule cron.CronSchedule

	// 检查 at_seconds（一次性）、every_seconds（重复）或 cron_expr
	atSeconds, hasAt := args["at_seconds"].(float64)
	everySeconds, hasEvery := args["every_seconds"].(float64)
	cronExpr, hasCron := args["cron_expr"].(string)

	// 修复：类型断言对零值也会返回 true，因此需要额外的有效性检查
	// 这样可防止把未使用的可选参数填成默认值（0）的 LLM 触发错误的类型。
	hasAt = hasAt && atSeconds > 0
	hasEvery = hasEvery && everySeconds > 0
	hasCron = hasCron && cronExpr != ""

	// 优先级：at_seconds > every_seconds > cron_expr
	if hasAt {
		atMS := time.Now().UnixMilli() + int64(atSeconds)*1000
		schedule = cron.CronSchedule{
			Kind: "at",
			AtMS: &atMS,
		}
	} else if hasEvery {
		everyMS := int64(everySeconds) * 1000
		schedule = cron.CronSchedule{
			Kind:    "every",
			EveryMS: &everyMS,
		}
	} else if hasCron {
		schedule = cron.CronSchedule{
			Kind: "cron",
			Expr: cronExpr,
		}
	} else {
		return ErrorResult("at_seconds、every_seconds 或 cron_expr 必须提供一个")
	}

	// GHSA-pv8c-p6jf-3fpp：调度命令需要内部 channel。当 allow_command 被禁用时，
	// 需要显式确认作为覆盖手段。非命令类提醒仍对所有 channel 开放。
	command, _ := args["command"].(string)
	commandConfirm, _ := args["command_confirm"].(bool)
	if command != "" {
		if !t.execEnabled {
			return ErrorResult("命令执行已被禁用")
		}
		if !constants.IsInternalChannel(channel) && !isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes) {
			return ErrorResult(
				"调度命令执行仅限内部 channel 或已配置的远程 channel",
			)
		}
		if !t.allowCommand && !commandConfirm {
			return ErrorResult("allow_command 关闭时必须设置 command_confirm=true")
		}
	}

	// 截断消息作为任务名（最多 30 个字符）
	messagePreview := utils.Truncate(message, 30)

	job, err := t.cronService.AddJob(
		messagePreview,
		schedule,
		message,
		channel,
		chatID,
	)
	if err != nil {
		return ErrorResult(fmt.Sprintf("添加定时任务失败：%v", err))
	}

	// 一次性应用可选的 payload 字段，并通过单次 UpdateJob 调用持久化
	needsUpdate := false
	if command != "" {
		job.Payload.Command = command
		needsUpdate = true
	}
	if needsUpdate {
		t.cronService.UpdateJob(job)
	}

	return SilentResult(fmt.Sprintf("定时任务已添加：%s (id: %s)", job.Name, job.ID))
}

func (t *CronTool) listJobs(ctx context.Context) *ToolResult {
	jobs := t.cronService.ListJobs(false)

	var accessibleJobs []cron.CronJob
	for _, job := range jobs {
		if t.canAccessJob(ctx, &job) {
			accessibleJobs = append(accessibleJobs, job)
		}
	}
	jobs = accessibleJobs

	if len(jobs) == 0 {
		return SilentResult("没有已安排的定时任务")
	}

	var result strings.Builder
	result.WriteString("已安排的定时任务：\n")
	for _, j := range jobs {
		var scheduleInfo string
		if j.Schedule.Kind == "every" && j.Schedule.EveryMS != nil {
			scheduleInfo = fmt.Sprintf("每 %d 秒", *j.Schedule.EveryMS/1000)
		} else if j.Schedule.Kind == "cron" {
			scheduleInfo = j.Schedule.Expr
		} else if j.Schedule.Kind == "at" {
			scheduleInfo = "一次性"
		} else {
			scheduleInfo = "未知"
		}
		result.WriteString(fmt.Sprintf("- %s (id: %s, %s)\n", j.Name, j.ID, scheduleInfo))
	}

	return SilentResult(result.String())
}

func (t *CronTool) getJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "get")
	if errResult != nil {
		return errResult
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("定时任务 %s 无法从当前 channel 访问", jobID))
	}

	return SilentResult(formatCronJobJSON(job))
}

func (t *CronTool) updateJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "update")
	if errResult != nil {
		return errResult
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("定时任务 %s 无法从当前 channel 访问", jobID))
	}

	patches := 0

	name, namePresent, nameErr := optionalNonEmptyString(args, "name")
	if nameErr != nil {
		return nameErr
	}
	if namePresent {
		job.Name = name
		patches++
	}

	message, messagePresent, messageErr := optionalNonEmptyString(args, "message")
	if messageErr != nil {
		return messageErr
	}
	if messagePresent {
		job.Payload.Message = message
		patches++
	}

	schedule, hasSchedule, errResult := schedulePatch(args)
	if errResult != nil {
		return errResult
	}
	if hasSchedule {
		job.Schedule = schedule
		job.DeleteAfterRun = schedule.Kind == "at"
		patches++
	}

	command, commandPresent, errResult := optionalString(args, "command")
	if errResult != nil {
		return errResult
	}
	if commandPresent {
		if errResult := t.validateCommandMutation(ctx, args); errResult != nil {
			return errResult
		}
		job.Payload.Command = command
		patches++
	}

	if patches == 0 {
		return ErrorResult("update 至少需要一个字段")
	}

	if err := t.cronService.UpdateJob(job); err != nil {
		return ErrorResult(fmt.Sprintf("更新定时任务失败：%v", err))
	}

	updated, _ := t.cronService.GetJob(jobID)
	return SilentResult(fmt.Sprintf("定时任务已更新：\n%s", formatCronJobJSON(updated)))
}

func (t *CronTool) removeJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("remove 操作需要 job_id")
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("定时任务 %s 无法从当前 channel 访问", jobID))
	}

	if t.cronService.RemoveJob(jobID) {
		return SilentResult(fmt.Sprintf("定时任务已移除：%s", jobID))
	}
	return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
}

func requiredCronJobID(args map[string]any, action string) (string, *ToolResult) {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return "", ErrorResult(fmt.Sprintf("%s 操作需要 job_id", action))
	}
	return jobID, nil
}

func optionalNonEmptyString(args map[string]any, key string) (string, bool, *ToolResult) {
	_, present := args[key]
	if !present {
		return "", false, nil
	}
	text, _, errResult := optionalString(args, key)
	if errResult != nil {
		return "", false, errResult
	}
	if strings.TrimSpace(text) == "" {
		return "", false, ErrorResult(fmt.Sprintf("%s 不能为空", key))
	}
	return text, true, nil
}

func optionalString(args map[string]any, key string) (string, bool, *ToolResult) {
	value, present := args[key]
	if !present {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, ErrorResult(fmt.Sprintf("%s 必须是字符串", key))
	}
	return text, true, nil
}

func schedulePatch(args map[string]any) (cron.CronSchedule, bool, *ToolResult) {
	var schedule cron.CronSchedule
	patches := 0

	if _, present := args["at_seconds"]; present {
		seconds, errResult := positiveSeconds(args, "at_seconds")
		if errResult != nil {
			return cron.CronSchedule{}, false, errResult
		}
		atMS := time.Now().UnixMilli() + seconds*1000
		schedule = cron.CronSchedule{Kind: "at", AtMS: &atMS}
		patches++
	}

	if _, present := args["every_seconds"]; present {
		seconds, errResult := positiveSeconds(args, "every_seconds")
		if errResult != nil {
			return cron.CronSchedule{}, false, errResult
		}
		everyMS := seconds * 1000
		schedule = cron.CronSchedule{Kind: "every", EveryMS: &everyMS}
		patches++
	}

	if _, present := args["cron_expr"]; present {
		cronExpr, ok := args["cron_expr"].(string)
		if !ok {
			return cron.CronSchedule{}, false, ErrorResult("cron_expr 必须是字符串")
		}
		if strings.TrimSpace(cronExpr) == "" {
			return cron.CronSchedule{}, false, ErrorResult("cron_expr 不能为空")
		}
		schedule = cron.CronSchedule{Kind: "cron", Expr: cronExpr}
		patches++
	}

	if patches > 1 {
		return cron.CronSchedule{}, false, ErrorResult("at_seconds、every_seconds、cron_expr 只能设置一个")
	}
	return schedule, patches == 1, nil
}

func positiveSeconds(args map[string]any, key string) (int64, *ToolResult) {
	value := args[key]
	var seconds int64
	switch v := value.(type) {
	case float64:
		if v != float64(int64(v)) {
			return 0, ErrorResult(fmt.Sprintf("%s 必须是正整数", key))
		}
		seconds = int64(v)
	case int:
		seconds = int64(v)
	case int64:
		seconds = v
	default:
		return 0, ErrorResult(fmt.Sprintf("%s 必须是正整数", key))
	}
	if seconds <= 0 {
		return 0, ErrorResult(fmt.Sprintf("%s 必须是正整数", key))
	}
	return seconds, nil
}

func (t *CronTool) validateCommandMutation(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)
	if !t.execEnabled {
		return ErrorResult("命令执行已被禁用")
	}
	if !constants.IsInternalChannel(channel) && !isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes) {
		return ErrorResult(
			"更新命令执行仅限内部 channel 或已配置的远程 channel",
		)
	}
	commandConfirm, _ := args["command_confirm"].(bool)
	if !t.allowCommand && !commandConfirm {
		return ErrorResult("allow_command 关闭时必须设置 command_confirm=true")
	}
	return nil
}

func isCommandAllowedRemote(channel, chatID string, allowed []string) bool {
	if channel == "" {
		return false
	}

	target := channel
	if chatID != "" {
		target = channel + ":" + chatID
	}

	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" || entry == channel || entry == target {
			return true
		}
	}

	return false
}

func (t *CronTool) canAccessJob(ctx context.Context, job *cron.CronJob) bool {
	channel := ToolChannel(ctx)
	if constants.IsInternalChannel(channel) {
		return true
	}

	chatID := ToolChatID(ctx)
	if channel == "" || chatID == "" {
		return false
	}
	if job.Payload.Channel != channel || job.Payload.To != chatID {
		return false
	}
	if job.Payload.Command != "" {
		return isCommandAllowedRemote(channel, chatID, t.commandAllowedRemotes)
	}
	return true
}

func formatCronJobJSON(job *cron.CronJob) string {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Sprintf("%+v", *job)
	}
	return string(data)
}

func (t *CronTool) enableJob(ctx context.Context, args map[string]any, enable bool) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("enable/disable 操作需要 job_id")
	}

	job, ok := t.cronService.GetJob(jobID)
	if !ok {
		return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("定时任务 %s 无法从当前 channel 访问", jobID))
	}

	updatedJob := t.cronService.EnableJob(jobID, enable)
	if updatedJob == nil {
		return ErrorResult(fmt.Sprintf("定时任务 %s 不存在", jobID))
	}

	status := "已启用"
	if !enable {
		status = "已禁用"
	}
	return SilentResult(fmt.Sprintf("定时任务 '%s' %s", updatedJob.Name, status))
}

// ExecuteJob 通过 agent 执行定时任务
func (t *CronTool) ExecuteJob(ctx context.Context, job *cron.CronJob) string {
	// 从任务 payload 中获取 channel/chatID
	channel := job.Payload.Channel
	chatID := job.Payload.To

	// 未设置时使用默认值
	if channel == "" {
		channel = "cli"
	}
	if chatID == "" {
		chatID = "direct"
	}

	// 如果存在命令则执行
	if job.Payload.Command != "" {
		if !t.execEnabled || t.execTool == nil {
			output := "执行定时命令失败：命令执行已被禁用"
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer pubCancel()
			t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
				Context: bus.NewOutboundContext(channel, chatID, ""),
				Content: output,
			})
			return "ok"
		}

		args := map[string]any{
			"action":    "run",
			"command":   job.Payload.Command,
			"__channel": channel,
			"__chat_id": chatID,
		}

		result := t.execTool.Execute(ctx, args)
		var output string
		if result.IsError {
			output = fmt.Sprintf("执行定时命令失败：%s", result.ForLLM)
		} else {
			output = fmt.Sprintf("定时命令 '%s' 已执行：\n%s", job.Payload.Command, result.ForLLM)
		}

		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
			Context: bus.NewOutboundContext(channel, chatID, ""),
			Content: output,
		})
		return "ok"
	}

	sessionKey := fmt.Sprintf("agent:cron-%s-%s", job.ID, uuid.New().String())

	// 使用任务消息调用 agent
	response, err := t.executor.ProcessDirectWithChannel(
		ctx,
		job.Payload.Message,
		sessionKey,
		channel,
		chatID,
	)
	if err != nil {
		return fmt.Sprintf("错误：%v", err)
	}

	if response != "" {
		t.executor.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, response, "", nil)
	}
	return "ok"
}
