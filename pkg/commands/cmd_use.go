package commands

func useCommand() Definition {
	return Definition{
		Name:        "use",
		Description: "为单次请求强制使用指定已安装技能",
		Usage:       "/use <skill> [message]",
	}
}
