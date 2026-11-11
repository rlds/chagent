package pid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
)

const pidFileName = ".chagent.pid"

var errInvalidPidFile = errors.New("invalid pid file")

// PidFileData 是存储在 PID 文件中的 JSON 结构。
type PidFileData struct {
	PID     int    `json:"pid"`
	Token   string `json:"token"`
	Version string `json:"version"`
	Port    int    `json:"port"`
	Host    string `json:"host"`
}

var pidMu sync.Mutex

// pidFilePath 根据 home 目录返回 PID 文件的绝对路径。
func pidFilePath(homePath string) string {
	return filepath.Join(homePath, pidFileName)
}

// generateToken 生成一个加密安全的 32 字符十六进制令牌。
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 若 crypto/rand 失败，则回退到伪随机值
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// WritePidFile 以原子方式创建（或覆盖）PID 文件。
// 如果看起来已有另一个网关实例在运行，则返回错误
// （存在有效的 PID 文件且对应进程仍然存活）。
func WritePidFile(homePath, host string, port int) (*PidFileData, error) {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)

	// 检查是否已存在 PID 文件 → 单实例约束。
	if data, err := readPidFileUnlocked(pidPath); err == nil {
		if os.Getpid() != data.PID {
			logger.Infof("found pid file (PID: %d, version: %s)", data.PID, data.Version)
			// PID 1 通常是宿主机上的 init/systemd，或容器内的入口进程。
			// 当容器停止并在共享卷上遗留 PID 文件时，宿主机的 PID 1（init）
			// 会通过 isProcessRunning 检查，从而阻止新的网关启动。
			// 因此记录为 PID 1 的文件一律视为陈旧。
			if data.PID != 1 && isProcessRunning(data.PID) {
				// 确认该进程确实是 chagent 实例。
				// 如果 PID 已被无关进程复用
				//（例如 kill -9 之后的 systemd-resolved），
				// 则将 PID 文件视为陈旧并继续启动。
				if isChagentProcess(data.PID) {
					return nil, fmt.Errorf("gateway is already running (PID: %d, version: %s)", data.PID, data.Version)
				}
				logger.Warnf("found pid file (PID: %d) but process is not chagent", data.PID)
			}
			logger.Warnf("not running (PID: %d) so will remove the pid file: %s", data.PID, pidPath)
		}
		// 陈旧的 PID 文件；进程已不存在 → 清理。
		os.Remove(pidPath)
	}

	data := &PidFileData{
		PID:     os.Getpid(),
		Version: config.GetVersion(),
		Port:    port,
		Host:    host,
	}

	token := generateToken()
	data.Token = token

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pid file: %w", err)
	}

	// 确保父目录存在。
	dir := filepath.Dir(pidPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create pid directory: %w", err)
	}

	// 通过临时文件 + 重命名实现原子写入。
	tmp := pidPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write pid file: %w", err)
	}
	if err := os.Rename(tmp, pidPath); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("failed to rename pid file: %w", err)
	}
	logger.Debugf("wrote pid file: %s success", pidPath)

	return data, nil
}

// ReadPidFileWithCheck 读取 PID 文件，并额外检查
// 所记录的进程是否仍然存活。如果文件不存在、
// 无法读取或进程已退出，则返回 nil。
func ReadPidFileWithCheck(homePath string) *PidFileData {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	data, err := readPidFileUnlocked(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if errors.Is(err, errInvalidPidFile) {
			logger.Warnf("invalid pid file, remove it: %s (%v)", pidPath, err)
			_ = os.Remove(pidPath)
			return nil
		}
		logger.Debugf("failed to read pid file: %s", err)
		return nil
	}

	// 当自身不是 PID 1 时，将 PID 1 视为陈旧（容器在共享卷上
	// 遗留的文件 —— 宿主机的 PID 1 是 init，而不是网关）。
	if data.PID == 1 && os.Getpid() != 1 {
		logger.Debugf("stale container PID 1, remove pid file: %s", pidPath)
		os.Remove(pidPath)
		return nil
	}

	if !isProcessRunning(data.PID) {
		logger.Debugf("process not running, remove pid file: %s", pidPath)
		os.Remove(pidPath)
		return nil
	}

	return data
}

// RemovePidFile 删除 PID 文件（例如在优雅关闭时）。
func RemovePidFile(homePath string) {
	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	// 仅当 PID 与自身进程匹配时才删除（避免删掉
	// 属于更新网关实例的文件）。
	if data, err := readPidFileUnlocked(pidPath); err == nil {
		if data.PID != os.Getpid() {
			return
		}
	}

	logger.Infof("remove pid file: %s", pidPath)
	os.Remove(pidPath)
}

// RemovePidFileIfPID 仅在记录的 PID 与 expectedPID 匹配时
// 删除 PID 文件。成功删除该文件时返回 true。
func RemovePidFileIfPID(homePath string, expectedPID int) bool {
	if expectedPID <= 0 {
		return false
	}

	pidMu.Lock()
	defer pidMu.Unlock()

	pidPath := pidFilePath(homePath)
	data, err := readPidFileUnlocked(pidPath)
	if err != nil {
		return false
	}
	if data.PID != expectedPID {
		return false
	}
	if err := os.Remove(pidPath); err != nil {
		return false
	}
	return true
}

// readPidFileUnlocked 在不获取锁的情况下读取 PID 文件。
// 调用方必须已持有 pidMu。
func readPidFileUnlocked(pidPath string) (*PidFileData, error) {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return nil, err
	}

	var data PidFileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidPidFile, err)
	}

	// 校验 PID 为正整数。
	if data.PID <= 0 {
		return nil, fmt.Errorf("%w: pid=%d", errInvalidPidFile, data.PID)
	}

	return &data, nil
}
