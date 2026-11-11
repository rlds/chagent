package commands

import (
	"context"
	"fmt"
	"strings"
)

func contextCommand() Definition {
	return Definition{
		Name:        "context",
		Description: "显示当前会话上下文与 token 用量",
		Usage:       "/context",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.GetContextStats == nil {
				return req.Reply(unavailableMsg)
			}
			stats := rt.GetContextStats()
			if stats == nil {
				return req.Reply("当前没有活动的会话上下文。")
			}
			return req.Reply(formatContextStats(stats))
		},
	}
}

func formatContextStats(s *ContextStats) string {
	remaining := s.CompressAtTokens - s.UsedTokens
	if remaining < 0 {
		remaining = 0
	}
	usedWindowPercent := s.UsedTokens * 100 / max(s.TotalTokens, 1)
	msg := fmt.Sprintf(
		"上下文用量  \n消息数：%d  \n已用：约 %d / %d tokens（%d%%）  \n历史：约 %d tokens  \n压缩阈值：%d tokens  \n摘要阈值：%d tokens  \n压缩进度：%d%%  \n剩余：约 %d tokens",
		s.MessageCount,
		s.UsedTokens,
		s.TotalTokens,
		usedWindowPercent,
		s.HistoryTokens,
		s.CompressAtTokens,
		s.SummarizeAtTokens,
		s.UsedPercent,
		remaining,
	)
	msg += formatWorkspaceContextStats(s.Workspace)
	msg += formatRuntimeContextStats(s.Runtime)
	msg += formatTaskContextStats(s.Task)
	return msg
}

func formatWorkspaceContextStats(w *WorkspaceContextStats) string {
	if w == nil {
		return "\n\n工作区指令：未加载。"
	}
	if !w.Enabled {
		return "\n\n工作区指令：已禁用。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n工作区指令：%d 个文件，约 %d 字节（用户级 %d，项目 %d），预算 %d 字节",
		w.Files, w.Bytes, w.UserGlobal, w.Project, w.Budget)
	if w.Omitted > 0 {
		fmt.Fprintf(&b, "，预算省略 %d", w.Omitted)
	}
	if w.Truncated > 0 {
		fmt.Fprintf(&b, "，截断 %d", w.Truncated)
	}
	for _, path := range w.Paths {
		fmt.Fprintf(&b, "\n  - %s", path)
	}
	if len(w.Paths) == 0 {
		b.WriteString("\n  - （无）")
	}
	return b.String()
}

func formatRuntimeContextStats(r *RuntimeContextStats) string {
	if r == nil {
		return "\n\n运行时上下文：未加载。"
	}
	mode := r.Mode
	if mode == "" {
		mode = "system"
	}
	if r.Snapshot == "" {
		return fmt.Sprintf("\n\n运行时上下文：模式 %s，刷新间隔 %d 秒。", mode, r.Interval)
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 600
	}
	return fmt.Sprintf("\n\n运行时上下文：模式 %s，快照 %s（刷新间隔 %d 秒）。", mode, r.Snapshot, interval)
}

func formatTaskContextStats(t *TaskContextStats) string {
	if t == nil {
		return "\n\n任务状态：无。"
	}
	return fmt.Sprintf("\n\n任务状态：%s，轮次 %d/%d，待办 %d（进行中 %d）。",
		t.Phase, t.Round, t.MaxRounds, t.Todos, t.InProgress)
}
