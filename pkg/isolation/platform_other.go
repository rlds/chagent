//go:build !linux && !windows

package isolation

import (
	"os/exec"

	"github.com/rlds/chagent/pkg/config"
)

func applyPlatformIsolation(cmd *exec.Cmd, isolation config.IsolationConfig, root string) error {
	// 目前不支持的平台保持命令不变。调用方依赖 Preflight
	// 和更高层级的检查来暴露不受支持的隔离模式。
	return nil
}

func postStartPlatformIsolation(cmd *exec.Cmd, isolation config.IsolationConfig, root string) error {
	return nil
}

func cleanupPendingPlatformResources(cmd *exec.Cmd) {
}
