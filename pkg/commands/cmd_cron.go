package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/cron"
)

const cronCommandUsage = "/cron [list|get <id>|add name=<名称> every_seconds=<秒>|cron_expr=<表达式> message=<内容>|remove <id>|enable <id>|disable <id>]"

// cronCommand 提供定时任务的查看、创建、启停和删除能力。
// 该命令只创建 agent 任务（message），不接受远程 shell command，避免从 Web 会话
// 绕过现有 cron 命令执行安全策略。
func cronCommand() Definition {
	return Definition{
		Name:        "cron",
		Description: "管理定时任务：列表、详情、新增、停止/开始、删除",
		Usage:       cronCommandUsage,
		SubCommands: []SubCommand{
			{Name: "list", Description: "列出全部定时任务", Handler: cronListHandler()},
			{Name: "get", Description: "查看定时任务详情", ArgsUsage: "<id>", Handler: cronGetHandler()},
			{Name: "add", Description: "新增定时任务（agent 任务）", ArgsUsage: "name=<名称> every_seconds=<秒>|cron_expr=<表达式> message=<内容>", Handler: cronAddHandler()},
			{Name: "remove", Description: "删除定时任务", ArgsUsage: "<id>", Handler: cronRemoveHandler()},
			{Name: "delete", Description: "删除定时任务（同 remove）", ArgsUsage: "<id>", Handler: cronRemoveHandler()},
			{Name: "enable", Description: "开始/启用定时任务", ArgsUsage: "<id>", Handler: cronToggleHandler(true)},
			{Name: "start", Description: "开始/启用定时任务（同 enable）", ArgsUsage: "<id>", Handler: cronToggleHandler(true)},
			{Name: "disable", Description: "停止/暂停定时任务", ArgsUsage: "<id>", Handler: cronToggleHandler(false)},
			{Name: "stop", Description: "停止/暂停定时任务（同 disable）", ArgsUsage: "<id>", Handler: cronToggleHandler(false)},
		},
	}
}

func cronServiceOrReply(req Request, rt *Runtime) *cron.CronService {
	if rt == nil || rt.Cron == nil {
		_ = req.Reply("当前 Agent 未启用定时任务服务。")
		return nil
	}
	return rt.Cron
}

func cronListHandler() Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		service := cronServiceOrReply(req, rt)
		if service == nil {
			return nil
		}
		jobs := service.ListJobs(true)
		if len(jobs) == 0 {
			return req.Reply("当前没有定时任务。\n使用 /cron add 创建任务。")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "定时任务（%d）\n", len(jobs))
		for _, job := range jobs {
			status := "已启用"
			if !job.Enabled {
				status = "已停止"
			}
			fmt.Fprintf(&b, "- %s  [%s]\n  %s\n  %s\n", job.ID, status, cronDisplayName(job), formatCronSchedule(job.Schedule))
		}
		b.WriteString("\n使用 /cron get <id> 查看详情。")
		return req.Reply(b.String())
	}
}

func cronGetHandler() Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		service := cronServiceOrReply(req, rt)
		if service == nil {
			return nil
		}
		id := nthToken(req.Text, 2)
		if id == "" {
			return req.Reply("用法：/cron get <id>")
		}
		job, ok := service.GetJob(id)
		if !ok {
			return req.Reply(fmt.Sprintf("未找到定时任务：%s", id))
		}
		return req.Reply(formatCronJobDetail(*job))
	}
}

func cronAddHandler() Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		service := cronServiceOrReply(req, rt)
		if service == nil {
			return nil
		}
		if strings.TrimSpace(req.Channel) == "" || strings.TrimSpace(req.ChatID) == "" {
			return req.Reply("当前会话缺少 channel/chat_id，无法创建定时任务。")
		}

		fields := parseCronFields(req.Text, 2)
		message := strings.TrimSpace(fields["message"])
		if message == "" {
			return req.Reply("缺少 message。示例：\n/cron add name=每日巡检 every_seconds=3600 message=\"检查系统状态\"")
		}
		if strings.TrimSpace(fields["command"]) != "" {
			return req.Reply("Web /cron 仅支持 agent 任务，不支持 shell command。请由 Agent 使用 cron 工具创建命令任务。")
		}

		schedule, err := cronScheduleFromFields(fields)
		if err != nil {
			return req.Reply("创建定时任务失败：" + err.Error())
		}

		name := strings.TrimSpace(fields["name"])
		if name == "" {
			name = truncateRunes(message, 30)
		}
		job, err := service.AddJob(name, schedule, message, req.Channel, req.ChatID)
		if err != nil {
			return req.Reply("创建定时任务失败：" + err.Error())
		}
		return req.Reply(fmt.Sprintf(
			"定时任务已创建。\nID：%s\n名称：%s\n调度：%s\n启用：是\n下次执行：%s",
			job.ID, job.Name, formatCronSchedule(job.Schedule), formatCronMillis(job.State.NextRunAtMS),
		))
	}
}

func cronRemoveHandler() Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		service := cronServiceOrReply(req, rt)
		if service == nil {
			return nil
		}
		id := nthToken(req.Text, 2)
		if id == "" {
			return req.Reply("用法：/cron remove <id>")
		}
		if !service.RemoveJob(id) {
			return req.Reply(fmt.Sprintf("未找到定时任务：%s", id))
		}
		return req.Reply(fmt.Sprintf("定时任务已删除：%s", id))
	}
}

func cronToggleHandler(enabled bool) Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		service := cronServiceOrReply(req, rt)
		if service == nil {
			return nil
		}
		id := nthToken(req.Text, 2)
		if id == "" {
			action := "enable"
			if !enabled {
				action = "disable"
			}
			return req.Reply(fmt.Sprintf("用法：/cron %s <id>", action))
		}
		job := service.EnableJob(id, enabled)
		if job == nil {
			return req.Reply(fmt.Sprintf("未找到定时任务：%s", id))
		}
		if enabled {
			return req.Reply(fmt.Sprintf("定时任务已开始：%s\n下次执行：%s", id, formatCronMillis(job.State.NextRunAtMS)))
		}
		return req.Reply(fmt.Sprintf("定时任务已停止：%s", id))
	}
}

func cronScheduleFromFields(fields map[string]string) (cron.CronSchedule, error) {
	if v, ok := fields["cron_expr"]; ok && strings.TrimSpace(v) != "" {
		return cron.CronSchedule{Kind: "cron", Expr: strings.TrimSpace(v)}, nil
	}
	if v, ok := fields["cron"]; ok && strings.TrimSpace(v) != "" {
		return cron.CronSchedule{Kind: "cron", Expr: strings.TrimSpace(v)}, nil
	}
	if v, ok := fields["at_seconds"]; ok && strings.TrimSpace(v) != "" {
		seconds, err := parseCronSeconds(v)
		if err != nil {
			return cron.CronSchedule{}, fmt.Errorf("at_seconds 无效: %w", err)
		}
		at := time.Now().UnixMilli() + seconds*1000
		return cron.CronSchedule{Kind: "at", AtMS: &at}, nil
	}
	if v, ok := fields["at"]; ok && strings.TrimSpace(v) != "" {
		seconds, err := parseCronSeconds(v)
		if err != nil {
			return cron.CronSchedule{}, fmt.Errorf("at 无效: %w", err)
		}
		at := time.Now().UnixMilli() + seconds*1000
		return cron.CronSchedule{Kind: "at", AtMS: &at}, nil
	}
	if v, ok := fields["every_seconds"]; ok && strings.TrimSpace(v) != "" {
		seconds, err := parseCronSeconds(v)
		if err != nil {
			return cron.CronSchedule{}, fmt.Errorf("every_seconds 无效: %w", err)
		}
		every := seconds * 1000
		return cron.CronSchedule{Kind: "every", EveryMS: &every}, nil
	}
	if v, ok := fields["every"]; ok && strings.TrimSpace(v) != "" {
		seconds, err := parseCronSeconds(v)
		if err != nil {
			return cron.CronSchedule{}, fmt.Errorf("every 无效: %w", err)
		}
		every := seconds * 1000
		return cron.CronSchedule{Kind: "every", EveryMS: &every}, nil
	}
	return cron.CronSchedule{}, fmt.Errorf("必须指定 at_seconds、every_seconds 或 cron_expr")
}

func parseCronSeconds(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("必须大于 0")
		}
		return n, nil
	}
	d, err := time.ParseDuration(value)
	if err == nil && d > 0 {
		return int64(d.Seconds()), nil
	}
	return 0, fmt.Errorf("需要秒数或 Go duration（如 10m、1h）")
}

// parseCronFields 解析 key=value 参数，支持双引号和单引号包裹空格。
// 只取 /cron <subcommand> 之后的参数。
func parseCronFields(text string, skip int) map[string]string {
	tokens := splitQuotedFields(text)
	fields := make(map[string]string)
	for i := 0; i < len(tokens); i++ {
		if i < skip {
			continue
		}
		key, value, found := strings.Cut(tokens[i], "=")
		if !found {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return fields
}

func splitQuotedFields(input string) []string {
	var out []string
	var current strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		default:
			if r == ' ' || r == '\t' || r == '\n' {
				flush()
			} else {
				current.WriteRune(r)
			}
		}
	}
	flush()
	return out
}

func formatCronJobDetail(job cron.CronJob) string {
	status := "已启用"
	if !job.Enabled {
		status = "已停止"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "定时任务详情\n")
	fmt.Fprintf(&b, "ID：%s\n", job.ID)
	fmt.Fprintf(&b, "名称：%s\n", cronDisplayName(job))
	fmt.Fprintf(&b, "状态：%s\n", status)
	fmt.Fprintf(&b, "调度：%s\n", formatCronSchedule(job.Schedule))
	fmt.Fprintf(&b, "下次执行：%s\n", formatCronMillis(job.State.NextRunAtMS))
	fmt.Fprintf(&b, "上次执行：%s\n", formatCronMillis(job.State.LastRunAtMS))
	if job.State.LastStatus != "" {
		fmt.Fprintf(&b, "上次状态：%s\n", job.State.LastStatus)
	}
	if job.State.LastError != "" {
		fmt.Fprintf(&b, "上次错误：%s\n", job.State.LastError)
	}
	fmt.Fprintf(&b, "目标通道：%s\n", job.Payload.Channel)
	fmt.Fprintf(&b, "目标会话：%s\n", job.Payload.To)
	fmt.Fprintf(&b, "任务内容：%s", job.Payload.Message)
	return b.String()
}

func formatCronSchedule(schedule cron.CronSchedule) string {
	switch schedule.Kind {
	case "at":
		return "一次性，执行时间 " + formatCronMillis(schedule.AtMS)
	case "every":
		if schedule.EveryMS == nil {
			return "固定间隔（未配置）"
		}
		return fmt.Sprintf("每 %s", (time.Duration(*schedule.EveryMS) * time.Millisecond).String())
	case "cron":
		if schedule.TZ != "" {
			return fmt.Sprintf("cron: %s (%s)", schedule.Expr, schedule.TZ)
		}
		return "cron: " + schedule.Expr
	default:
		return schedule.Kind
	}
}

func formatCronMillis(ms *int64) string {
	if ms == nil || *ms <= 0 {
		return "无"
	}
	return time.UnixMilli(*ms).Format("2006-01-02 15:04:05")
}

func cronDisplayName(job cron.CronJob) string {
	if strings.TrimSpace(job.Name) != "" {
		return job.Name
	}
	return truncateRunes(job.Payload.Message, 40)
}

func truncateRunes(value string, maxLen int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxLen {
		return string(runes)
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}
