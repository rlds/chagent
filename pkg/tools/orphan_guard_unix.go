//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// startOrphanGuard 启动一个极小的分离式 shell 看门狗，它持有管道的读端，
// 而写端保留在网关进程中。如果网关被 SIGKILL（或未关闭就退出），
// 内核会关闭写端，看门狗收到 EOF 并在退出前终止目标进程组。
// 这覆盖了 Setpgid + 信号处理器无法覆盖的 SIGKILL 缺口。
//
// pgid 必须是目标命令的进程组 id（使用 Setpgid 时是子进程 pid；
// 使用 Setsid/PTY 时是会话 pid）。返回的 stop 函数会关闭管道并等待看门狗；
// 它是幂等的。
func startOrphanGuard(pgid int) (func(), error) {
	if pgid <= 0 {
		return func() {}, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// 一直读取，直到父进程关闭管道（正常退出或通过 SIGKILL），
	// 然后终止进程组并给它一小段宽限期。
	const script = `cat >/dev/null 2>&1
kill -TERM -"$1" 2>/dev/null
kill -KILL -"$1" 2>/dev/null
`
	cmd := exec.Command("/bin/sh", "-c", script, "sh", strconv.Itoa(pgid))
	cmd.Stdin = r
	// 让看门狗拥有自己的进程组，这样目标进程组的 kill 不会在它完成前把它一起杀掉。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = r.Close()

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = w.Close()
			_ = cmd.Wait()
		})
	}, nil
}
