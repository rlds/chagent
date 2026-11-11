package media

import (
	"os"
	"path/filepath"
)

const TempDirName = "chagent_media"

// TempDir 返回用于存放已下载媒体的共享临时目录。
func TempDir() string {
	return filepath.Join(os.TempDir(), TempDirName)
}
