package commands

import "context"

func reloadCommand() Definition {
	return Definition{
		Name:        "reload",
		Description: "重新加载配置文件",
		Usage:       "/reload",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.ReloadConfig == nil {
				return req.Reply(unavailableMsg)
			}
			if err := rt.ReloadConfig(); err != nil {
				return req.Reply("重新加载配置失败：" + err.Error())
			}
			return req.Reply("已触发配置重载！")
		},
	}
}
