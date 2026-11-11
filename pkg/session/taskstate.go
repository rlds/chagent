package session

import (
	"fmt"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/taskstate"
)

// GetTaskState 为旧版内存型 SessionManager 实现 taskstate.Store。
// 任务状态与会话存放在一起，并由 Save 写入旧版 JSON 会话文件。
func (sm *SessionManager) GetTaskState(key string) (taskstate.State, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok || session.TaskState == nil || session.TaskState.IsZero() {
		return taskstate.State{}, false
	}
	return session.TaskState.Clone(), true
}

// SetTaskState 为 SessionManager 实现 taskstate.Store。零值状态会清除
// 已存储的任务状态。调用方通过 Save 将其持久化到磁盘。
func (sm *SessionManager) SetTaskState(key string, state taskstate.State) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if !ok {
		session = &Session{
			Key:      key,
			Messages: []providers.Message{},
			Created:  time.Now(),
		}
		sm.sessions[key] = session
	}

	if state.IsZero() {
		session.TaskState = nil
	} else {
		if err := state.Validate(); err != nil {
			return fmt.Errorf("session: 无效任务状态：%w", err)
		}
		cloned := state.Clone()
		session.TaskState = &cloned
	}
	session.Updated = time.Now()
	return nil
}

// GetTaskState 通过委托给底层 memory 存储，为 JSONLBackend 实现
// taskstate.Store。会话键会先通过别名解析，因此任务状态会跟随规范会话。
func (b *JSONLBackend) GetTaskState(sessionKey string) (taskstate.State, bool) {
	sessionKey = b.resolveSessionKey(sessionKey)
	state, ok := b.store.GetTaskState(sessionKey)
	if !ok {
		return taskstate.State{}, false
	}
	return state.Clone(), true
}

// SetTaskState 为 JSONLBackend 实现 taskstate.Store。与发后即忘的
// SessionStore 写入不同，这里会返回错误，因为丢失任务状态会静默
// 破坏自主继续执行。
func (b *JSONLBackend) SetTaskState(sessionKey string, state taskstate.State) error {
	sessionKey = b.resolveSessionKey(sessionKey)
	if err := b.store.SetTaskState(sessionKey, state); err != nil {
		return fmt.Errorf("session: 写入任务状态：%w", err)
	}
	return nil
}

// 编译期断言：两个内置存储都支持持久任务状态。
var (
	_ taskstate.Store = (*SessionManager)(nil)
	_ taskstate.Store = (*JSONLBackend)(nil)
	_ TaskStateStore  = (*SessionManager)(nil)
	_ TaskStateStore  = (*JSONLBackend)(nil)
)
