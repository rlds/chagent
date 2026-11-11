package version

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rlds/chagent/cmd/chagent/internal"
	"github.com/rlds/chagent/pkg/config"
)

func NewVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "version",
		Aliases: []string{"v"},
		Short:   "显示版本信息",
		Run: func(_ *cobra.Command, _ []string) {
			printVersion()
		},
	}

	return cmd
}

func printVersion() {
	build, goVer := config.FormatBuildInfo()
	// 纯文本输出；取代了已移除的 internal/cliui 渲染器。
	fmt.Printf("%s %s\n", internal.Logo, "chagent "+config.FormatVersion())
	if build != "" {
		fmt.Printf("  构建时间：%s\n", build)
	}
	if goVer != "" {
		fmt.Printf("  Go 版本：%s\n", goVer)
	}
}
