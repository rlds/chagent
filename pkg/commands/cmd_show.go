package commands

import (
	"context"
	"fmt"
)

func showCommand() Definition {
	return Definition{
		Name:        "show",
		Description: "显示当前配置",
		SubCommands: []SubCommand{
			{
				Name:        "model",
				Description: "当前模型与 provider",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.GetModelInfo == nil {
						return req.Reply(unavailableMsg)
					}
					name, provider := rt.GetModelInfo()
					return req.Reply(fmt.Sprintf("当前模型：%s（Provider：%s）", name, provider))
				},
			},
			{
				Name:        "channel",
				Description: "当前 channel",
				Handler: func(_ context.Context, req Request, _ *Runtime) error {
					return req.Reply(fmt.Sprintf("当前 channel：%s", req.Channel))
				},
			},
			{
				Name:        "agents",
				Description: "已注册 agent",
				Handler:     agentsHandler(),
			},
			{
				Name:        "mcp",
				Description: "某个 MCP server 的活跃工具",
				ArgsUsage:   "<server>",
				Handler:     showMCPToolsHandler(),
			},
		},
	}
}
