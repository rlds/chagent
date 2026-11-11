package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/logger"
)

// State 表示某个工作区的持久化状态。
// 其中包含最近使用的渠道/聊天相关的信息。
type State struct {
	// LastChannel 是最近用于通信的渠道
	LastChannel string `json:"last_channel,omitempty"`

	// LastChatID 是最近用于通信的聊天 ID
	LastChatID string `json:"last_chat_id,omitempty"`

	// Timestamp 是该状态最近一次更新的时间
	Timestamp time.Time `json:"timestamp"`
}

// Manager 管理持久化状态，并采用原子方式保存。
type Manager struct {
	workspace string
	state     *State
	mu        sync.RWMutex
	stateFile string
}

// NewManager 为指定的工作区创建一个新的状态管理器。
func NewManager(workspace string) *Manager {
	stateDir := filepath.Join(workspace, "state")
	stateFile := filepath.Join(stateDir, "state.json")
	oldStateFile := filepath.Join(workspace, "state.json")

	// 如果状态目录不存在，则创建该目录
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		logger.WarnCF("state", "failed to create state directory", map[string]any{
			"dir":   stateDir,
			"error": err.Error(),
		})
	}

	sm := &Manager{
		workspace: workspace,
		stateFile: stateFile,
		state:     &State{},
	}

	// 优先尝试从新位置加载
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		// 新文件不存在，尝试从旧位置迁移
		if data, err := os.ReadFile(oldStateFile); err == nil {
			if err := json.Unmarshal(data, sm.state); err == nil {
				// 迁移到新位置
				if err := sm.saveAtomic(); err != nil {
					logger.WarnCF("state", "failed to save state", map[string]any{
						"error": err.Error(),
					})
				}
				logger.InfoCF("state", "migrated state", map[string]any{
					"from": oldStateFile,
					"to":   stateFile,
				})
			}
		}
	} else {
		// 从新位置加载
		if err := sm.load(); err != nil {
			logger.WarnCF("state", "failed to load state", map[string]any{
				"error": err.Error(),
			})
		}
	}

	return sm
}

// SetLastChannel 以原子方式更新最近一次使用的渠道，并保存状态。
// 该方法采用临时文件 + 重命名的方式实现原子写入，
// 从而保证即使进程崩溃，状态文件也不会损坏。
func (sm *Manager) SetLastChannel(channel string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 更新状态
	sm.state.LastChannel = channel
	sm.state.Timestamp = time.Now()

	// 使用临时文件 + 重命名进行原子保存
	if err := sm.saveAtomic(); err != nil {
		return fmt.Errorf("failed to save state atomically: %w", err)
	}

	return nil
}

// SetLastChatID 以原子方式更新最近使用的聊天 ID，并保存状态。
func (sm *Manager) SetLastChatID(chatID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 更新状态
	sm.state.LastChatID = chatID
	sm.state.Timestamp = time.Now()

	// 使用临时文件 + 重命名进行原子保存
	if err := sm.saveAtomic(); err != nil {
		return fmt.Errorf("failed to save state atomically: %w", err)
	}

	return nil
}

// GetLastChannel 返回状态中记录的最近一次使用的渠道。
func (sm *Manager) GetLastChannel() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state.LastChannel
}

// GetLastChatID 返回状态中记录的最近一次使用的聊天 ID。
func (sm *Manager) GetLastChatID() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state.LastChatID
}

// GetTimestamp 返回状态最近一次更新的时间戳。
func (sm *Manager) GetTimestamp() time.Time {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state.Timestamp
}

// saveAtomic 使用临时文件 + 重命名执行原子保存。
// 这样可以确保状态文件永远不会损坏：
// 1. 写入临时文件
// 2. 同步到磁盘（对 SD 卡/闪存存储至关重要）
// 3. 将临时文件重命名为目标文件（在 POSIX 系统上是原子操作）
// 4. 如果重命名失败，则清理临时文件
//
// 调用时必须已持有锁。
func (sm *Manager) saveAtomic() error {
	// 使用统一的原子写入工具，并显式同步，以保证闪存存储的可靠性。
	// 采用 0o600（仅所有者可读写）作为安全的默认权限。
	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	return fileutil.WriteFileAtomic(sm.stateFile, data, 0o600)
}

// load 从磁盘加载状态。
func (sm *Manager) load() error {
	data, err := os.ReadFile(sm.stateFile)
	if err != nil {
		// 文件尚不存在，这种情况没有问题
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	if err := json.Unmarshal(data, sm.state); err != nil {
		return fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return nil
}
