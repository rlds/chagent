//go:build !windows

package pid

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// isProcessRunning 在类 Unix 系统上使用 signal(0) 检查
// 给定 PID 的进程是否存活。
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// 在 Unix 上，Signal(0) 不会杀死进程，只用于检查其是否存在。
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	var errno syscall.Errno
	// EPERM 表示进程存在，但无权向其发送信号。
	return errors.As(err, &errno) && errno == syscall.EPERM
}

// isChagentProcess 读取 /proc/<pid>/comm，以确认该进程属于
// Chagent 家族的网关（"chagent" 或国际象棋二进制 "chagent"）。
// 若能读取 comm 文件但名称不匹配（例如 PID 被无关进程复用），
// 则返回 false。若 /proc/<pid>/comm 无法读取，则返回 true，
// 以便调用方退化为仅依赖存活检查。
func isChagentProcess(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return true // 无法验证 —— 信任存活检查
	}
	return isChagentProcessName(string(data))
}
