//go:build windows

package jobs

import (
	"os"
	"os/exec"
	"syscall"
)

// shellExecutable 是 Windows 上用于运行 job 命令的 shell。
func shellExecutable() string { return "cmd" }

// shellArgs 将一条命令字符串包装为 cmd.exe 参数。
func shellArgs(command string) []string { return []string{"/c", command} }

// configureProcessGroup 请求 Windows 创建新的进程组，从而至少让组级终止请求
// 具备可表达的形式。Go 的 os/exec 无法以可移植的方式暴露控制台控制事件，因此
// terminateProcessGroup 退化为只终止直接子进程（这是相对于 POSIX 的已知局限）。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateProcessGroup 终止该 job 的直接子进程。没有控制台句柄时，Go 无法以
// 可移植的方式发送优雅停止信号（Ctrl+Break），因此 SIGTERM 式请求与强制请求
// 最终都会走到 Process.Kill；管理器仍会将该 job 报告为已被终止，并等待 Wait 完成。
func terminateProcessGroup(pid int, force bool) {
	if pid <= 0 {
		return
	}
	proc, err := findProcess(pid)
	if err != nil || proc == nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
	_ = proc.Kill()
}

// processAlive 报告 pid 是否指向一个存活进程。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc == nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// findProcess 返回 pid 对应的操作系统进程句柄。
func findProcess(pid int) (*os.Process, error) {
	return os.FindProcess(pid)
}
