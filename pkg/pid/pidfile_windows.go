//go:build windows

package pid

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess         = kernel32.NewProc("GetExitCodeProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	processQueryLimitedInformation = uint32(0x1000)
	stillActive                    = uint32(259)
)

// isProcessRunning 在 Windows 上使用 OpenProcess + GetExitCodeProcess
// 检查给定 PID 的进程是否存活。
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	handle, _, _ := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation),
		0,
		uintptr(pid),
	)
	if handle == 0 {
		return false
	}
	defer procCloseHandle.Call(handle)

	var exitCode uint32
	ret, _, _ := procGetExitCodeProcess.Call(handle, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 {
		return false
	}
	return exitCode == stillActive
}

// isChagentProcess 使用 QueryFullProcessImageNameW 确认
// 进程映像名称包含 "chagent"。若名称明显不匹配则返回 false。
// 若查询失败，则返回 true，退化为仅依赖存活检查。
func isChagentProcess(pid int) bool {
	handle, _, _ := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation),
		0,
		uintptr(pid),
	)
	if handle == 0 {
		return true // 无法打开 —— 信任存活检查
	}
	defer procCloseHandle.Call(handle)

	var buf [260]uint16
	var size uint32 = 260
	ret, _, _ := procQueryFullProcessImageNameW.Call(
		uintptr(handle),
		0, // WIN32_NAME_FORMAT
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 {
		return true // 无法验证 —— 信任存活检查
	}
	name := syscall.UTF16ToString(buf[:size])
	return isChagentProcessName(name)
}
