//go:build windows

package tools

// startOrphanGuard 在 Windows 上是空操作：进程树终止路径使用
// taskkill /T，且 Windows 没有可移植的父进程死亡信号。
func startOrphanGuard(int) (func(), error) {
	return func() {}, nil
}
