//go:build !windows

package jobs

import (
	"os"
	"os/exec"
	"syscall"
)

// shellExecutable 是 POSIX 系统上用于运行任务命令的 shell。
func shellExecutable() string { return "/bin/sh" }

// shellArgs 把单个命令字符串包装为 POSIX shell 参数。
func shellArgs(command string) []string { return []string{"-c", command} }

// configureProcessGroup 让任务在独立的进程组中启动，使 job_kill 能终止
// 整棵进程树（shell 及其子进程），而不只是 shell 包装层。这是进程层面
// DSH 语义的主要差异：DSH 的本地任务 runner 通过自己的 supervisor 终止子进程树。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processAlive 报告 pid 是否对应一个存活进程（非僵尸、仍可被信号送达）。
// 它仅用于 manager 启动时清理过期任务。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// terminateProcessGroup 向任务的进程组发送信号。force=false 发送 SIGTERM，
// force=true 发送 SIGKILL；当进程组 id 不可用时（例如组长已退出、
// 子进程被重新托管），两者都会退化为只向单个进程发送信号。
func terminateProcessGroup(pid int, force bool) {
	if pid <= 0 {
		return
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, signal); err == nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil && proc != nil {
		_ = proc.Signal(signal)
	}
}
