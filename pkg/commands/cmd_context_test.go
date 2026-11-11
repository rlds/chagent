package commands

import (
	"strings"
	"testing"
)

func TestFormatContextStatsChineseSections(t *testing.T) {
	msg := formatContextStats(&ContextStats{
		UsedTokens: 100, TotalTokens: 1000, HistoryTokens: 80,
		CompressAtTokens: 800, SummarizeAtTokens: 900, UsedPercent: 10, MessageCount: 3,
		Workspace: &WorkspaceContextStats{
			Enabled: true, Files: 2, Bytes: 120, Budget: 32768, Project: 2,
			Paths: []string{"AGENTS.md", "packages/app/AGENTS.md"},
		},
		Runtime: &RuntimeContextStats{Mode: "snapshot", Interval: 600, Snapshot: "2026-10-29T10:30:00+08:00"},
		Task:    &TaskContextStats{Phase: "active", Round: 2, MaxRounds: 20, Todos: 3, InProgress: 1},
	})
	for _, want := range []string{"上下文用量", "工作区指令", "AGENTS.md", "运行时上下文", "任务状态", "轮次 2/20"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("context stats missing %q:\n%s", want, msg)
		}
	}
}

func TestFormatContextStatsNoWorkspace(t *testing.T) {
	msg := formatContextStats(&ContextStats{TotalTokens: 1000})
	if !strings.Contains(msg, "工作区指令：未加载") {
		t.Fatalf("missing no-workspace message:\n%s", msg)
	}
}
