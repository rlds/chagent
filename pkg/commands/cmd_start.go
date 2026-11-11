package commands

import "context"

func startCommand() Definition {
	return Definition{
		Name:        "start",
		Description: "启动机器人",
		Usage:       "/start",
		Handler: func(_ context.Context, req Request, _ *Runtime) error {
			return req.Reply("你好！我是 Chagent 🦞")
		},
	}
}
