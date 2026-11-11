package commands

import (
	"context"
	"fmt"
	"strings"
)

func stopCommand() Definition {
	return Definition{
		Name:        "stop",
		Description: "停止当前任务",
		Usage:       "/stop",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.StopActiveTurn == nil {
				return req.Reply(unavailableMsg)
			}

			result, err := rt.StopActiveTurn()
			if err != nil {
				return req.Reply("停止任务失败：" + err.Error())
			}

			return req.Reply(FormatStopReply(result))
		},
	}
}

// FormatStopReply 为停止请求渲染面向用户的回复。
func FormatStopReply(result StopResult) string {
	if !result.Stopped {
		return "没有可停止的活跃任务。"
	}

	taskName := compactStopTaskName(result.TaskName)
	if taskName == "" {
		return "任务已停止。当前任务已取消。"
	}

	return fmt.Sprintf("任务已停止。%q 已取消。", taskName)
}

func compactStopTaskName(taskName string) string {
	taskName = strings.Join(strings.Fields(strings.TrimSpace(taskName)), " ")
	if taskName == "" {
		return ""
	}
	if len(taskName) > 80 {
		return taskName[:77] + "..."
	}
	return taskName
}
