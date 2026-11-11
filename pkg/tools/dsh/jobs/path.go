package jobs

import (
	"path/filepath"
	"runtime"
	"strings"
)

// cleanPath 在不访问文件系统的情况下规范化绝对路径。
// 相对路径返回 ""，因为调用方只会传入绝对路径候选值。
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// pathHasPrefix 报告 path 是否等于 root 或位于 root 之下。Windows
// 文件系统路径按大小写不敏感比较，POSIX 路径按精确比较。
func pathHasPrefix(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
