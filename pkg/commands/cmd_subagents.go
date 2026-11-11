package commands

import (
	"context"
	"fmt"
)

// TurnInfo 是从 agent.TurnInfo 镜像而来的结构体，用于避免循环依赖。
type TurnInfo struct {
	TurnID       string
	ParentTurnID string
	Depth        int
	ChildTurnIDs []string
	IsFinished   bool
}

func subagentsCommand() Definition {
	return Definition{
		Name:        "subagents",
		Description: "显示运行中的 subagent 与任务树",
		Handler: func(ctx context.Context, req Request, rt *Runtime) error {
			getTurnFn := rt.GetActiveTurn
			if getTurnFn == nil {
				return req.Reply("运行时不支持查询活跃回合。")
			}

			turnRaw := getTurnFn()
			if turnRaw == nil {
				return req.Reply("当前会话没有运行中的任务。")
			}

			if treeStr, ok := turnRaw.(string); ok {
				if treeStr == "" {
					return req.Reply("当前会话没有运行中的任务。")
				}
				return req.Reply(fmt.Sprintf("🤖 **活跃 Subagent 树**\n```text\n%s\n```", treeStr))
			}

			return req.Reply(fmt.Sprintf("🤖 **活跃 Subagent 列表**\n```text\n%+v\n```", turnRaw))
		},
	}
}
