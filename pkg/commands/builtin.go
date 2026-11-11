package commands

// BuiltinDefinitions 返回所有内置命令定义。
// 每个命令组都定义在各自的 cmd_*.go 文件中。
// 定义是无状态的——运行时依赖通过执行时传递给处理器的
// Runtime 参数提供。
func BuiltinDefinitions() []Definition {
	return []Definition{
		startCommand(),
		helpCommand(),
		stopCommand(),
		showCommand(),
		listCommand(),
		useCommand(),
		btwCommand(),
		switchCommand(),
		checkCommand(),
		clearCommand(),
		contextCommand(),
		subagentsCommand(),
		reloadCommand(),
	}
}
