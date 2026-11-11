package commands

import (
	"context"
	"fmt"
	"strings"
)

func listCommand() Definition {
	return Definition{
		Name:        "list",
		Description: "列出可用选项",
		SubCommands: []SubCommand{
			{
				Name:        "models",
				Description: "已配置模型",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.GetModelInfo == nil {
						return req.Reply(unavailableMsg)
					}
					name, provider := rt.GetModelInfo()
					if provider == "" {
						provider = "配置的默认值"
					}
					return req.Reply(fmt.Sprintf(
						"已配置模型：%s\nProvider：%s\n\n如需切换模型，请修改 config.json",
						name, provider,
					))
				},
			},
			{
				Name:        "channels",
				Description: "已启用的 channel",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.GetEnabledChannels == nil {
						return req.Reply(unavailableMsg)
					}
					enabled := rt.GetEnabledChannels()
					if len(enabled) == 0 {
						return req.Reply("没有启用的 channel")
					}
					return req.Reply(fmt.Sprintf("已启用 channel：\n- %s", strings.Join(enabled, "\n- ")))
				},
			},
			{
				Name:        "agents",
				Description: "已注册 agent",
				Handler:     agentsHandler(),
			},
			{
				Name:        "skills",
				Description: "已安装技能",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.ListSkillNames == nil {
						return req.Reply(unavailableMsg)
					}
					names := rt.ListSkillNames()
					if len(names) == 0 {
						return req.Reply("未安装任何技能")
					}
					return req.Reply(fmt.Sprintf(
						"已安装技能：\n- %s\n\n使用 /use <skill> <message> 可为单次请求强制使用某个技能，或使用 /use <skill> 将其应用到下一条消息。",
						strings.Join(names, "\n- "),
					))
				},
			},
			{
				Name:        "mcp",
				Description: "已配置 MCP server",
				Handler:     listMCPServersHandler(),
			},
		},
	}
}
