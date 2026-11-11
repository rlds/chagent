//go:build unix

package workspacecontext

import (
	"os"
	"syscall"
)

// versionFromFileInfo 构建 FileVersion，并在平台暴露 inode 时
// 一并包含它。
func versionFromFileInfo(info os.FileInfo) FileVersion {
	version := FileVersion{
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		version.Inode = uint64(stat.Ino)
	}
	return version
}
