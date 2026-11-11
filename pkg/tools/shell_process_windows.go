//go:build windows

package tools

import (
	"os/exec"
	"strconv"
)

func prepareCommandForTermination(cmd *exec.Cmd) {
	// 在 Windows 上为空操作
}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid
	if pid <= 0 {
		return nil
	}

	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	_ = cmd.Process.Kill()
	return nil
}
