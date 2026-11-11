//go:build windows

package tools

import "os/exec"

func setSysProcAttrForPty(cmd *exec.Cmd) {
	// Windows 不支持 Setsid，而且 Windows 上本来也没有 PTY。
	// 对 Windows 构建，该函数是空操作。
}
