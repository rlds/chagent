package commands

import (
	"context"
	"fmt"
)

func switchCommand() Definition {
	return Definition{
		Name:        "switch",
		Description: "切换模型",
		SubCommands: []SubCommand{
			{
				Name:        "model",
				Description: "切换到其他模型",
				ArgsUsage:   "to <name>",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.SwitchModel == nil {
						return req.Reply(unavailableMsg)
					}
					// 解析：/switch model to <value>
					value := nthToken(req.Text, 3) // 分词：[/switch, model, to, <value>]
					if nthToken(req.Text, 2) != "to" || value == "" {
						return req.Reply("用法：/switch model to <name>")
					}
					oldModel, err := rt.SwitchModel(value)
					if err != nil {
						return req.Reply(err.Error())
					}
					return req.Reply(fmt.Sprintf("模型已从 %s 切换为 %s", oldModel, value))
				},
			},
			{
				Name:        "channel",
				Description: "已迁移到 /check channel",
				Handler: func(_ context.Context, req Request, _ *Runtime) error {
					return req.Reply("该命令已迁移，请使用：/check channel <name>")
				},
			},
		},
	}
}
