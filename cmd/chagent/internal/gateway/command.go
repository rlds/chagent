package gateway

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/rlds/chagent/cmd/chagent/internal"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/gateway"
)

// NewGatewayCommand 启动国际象棋基础 agent 网关。仅对外暴露 --name 参数：
// 当 config.json 中包含 YOUR_PICO_TOKEN 占位符时，由它来选择 pico token。
// 其余所有行为均来自 config.json 的默认值（主机/端口、日志级别、模型、工具）。
func NewGatewayCommand() *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "启动 chagent gateway",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// --name 是可选的。空名称同样有效：当配置使用占位符时，
			// 会根据它推导出稳定的 "default" pico token。
			_ = os.Setenv(config.AgentNameEnv, name)
			return gateway.Run(false, internal.GetChagentHome(), internal.GetConfigPath(), false)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "agent 名称；当 config.json 使用 YOUR_PICO_TOKEN 占位符时用于推导 pico token")
	return cmd
}
