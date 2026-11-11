//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

func prepareCommandForTermination(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid
	if pid <= 0 {
		return nil
	}

	// 终止由 shell 命令派生出的整个进程组。
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// 兜底：再终止 shell 进程本身。
	_ = cmd.Process.Kill()
	return nil
}
