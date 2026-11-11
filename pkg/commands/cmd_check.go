package commands

import (
	"context"
	"fmt"
)

func checkCommand() Definition {
	return Definition{
		Name:        "check",
		Description: "检查 channel 可用性",
		SubCommands: []SubCommand{
			{
				Name:        "channel",
				Description: "检查某个 channel 是否可用",
				ArgsUsage:   "<name>",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.SwitchChannel == nil {
						return req.Reply(unavailableMsg)
					}
					value := nthToken(req.Text, 2)
					if value == "" {
						return req.Reply("用法：/check channel <name>")
					}
					if err := rt.SwitchChannel(value); err != nil {
						return req.Reply(err.Error())
					}
					return req.Reply(fmt.Sprintf("channel「%s」可用且已启用", value))
				},
			},
		},
	}
}
