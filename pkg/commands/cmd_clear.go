package commands

import "context"

func clearCommand() Definition {
	return Definition{
		Name:        "clear",
		Description: "清空聊天历史",
		Usage:       "/clear",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.ClearHistory == nil {
				return req.Reply(unavailableMsg)
			}
			if err := rt.ClearHistory(); err != nil {
				return req.Reply("清空聊天历史失败：" + err.Error())
			}
			return req.Reply("聊天历史已清空！")
		},
	}
}
