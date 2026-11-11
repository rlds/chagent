//go:build !unix

package workspacecontext

import "os"

// versionFromFileInfo 构建一个 FileVersion。没有可移植 inode 的平台会把
// Inode 留为零；此时仍可依靠大小和修改时间来标识版本。
func versionFromFileInfo(info os.FileInfo) FileVersion {
	return FileVersion{
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
}
