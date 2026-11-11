package commands

import (
	"fmt"
	"strings"
)

// SubCommand 定义一个父命令中的单个子命令。
type SubCommand struct {
	Name        string
	Description string
	ArgsUsage   string // 可选，例如 "<session-id>"
	Handler     Handler
}

// Definition 是斜杠命令的单一来源元数据与行为契约。
//
// 设计说明（第一阶段）：
//   - 每个 channel 都从此类型读取命令结构，而不再各自保留本地副本。
//   - 可见性是全局的：所有定义都被视为对所有 channel 可用。
//   - 平台菜单注册（例如 Telegram BotCommand）也派生自同一份定义，
//     从而保证 UI 标签与运行时行为保持一致。
type Definition struct {
	Name        string
	Description string
	Usage       string // 用于简单命令；设置 SubCommands 时忽略
	Aliases     []string
	SubCommands []SubCommand // 可选；设置后 Executor 会路由到子命令处理器
	Handler     Handler      // 用于没有子命令的简单命令
}

// EffectiveUsage 返回用法字符串。存在 SubCommands 时，
// 它会根据子命令名称自动生成，因此元数据与行为
// 不会产生漂移。
func (d Definition) EffectiveUsage() string {
	if len(d.SubCommands) == 0 {
		return d.Usage
	}
	names := make([]string, 0, len(d.SubCommands))
	for _, sc := range d.SubCommands {
		name := sc.Name
		if sc.ArgsUsage != "" {
			name += " " + sc.ArgsUsage
		}
		names = append(names, name)
	}
	return fmt.Sprintf("/%s [%s]", d.Name, strings.Join(names, "|"))
}
