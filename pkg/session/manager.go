package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/messageutil"
	"github.com/rlds/chagent/pkg/taskstate"
)

type Session struct {
	Key       string              `json:"key"`
	Messages  []providers.Message `json:"messages"`
	Summary   string              `json:"summary,omitempty"`
	Created   time.Time           `json:"created"`
	Updated   time.Time           `json:"updated"`
	TaskState *taskstate.State    `json:"task_state,omitempty"`
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	storage  string
}

func NewSessionManager(storage string) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*Session),
		storage:  storage,
	}

	if storage != "" {
		os.MkdirAll(storage, 0o700)
		sm.loadSessions()
	}

	return sm
}

func (sm *SessionManager) GetOrCreate(key string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		return session
	}

	session = &Session{
		Key:      key,
		Messages: []providers.Message{},
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	sm.sessions[key] = session

	return session
}

func ensureMessageCreatedAt(msg *providers.Message, fallback time.Time) {
	if msg.CreatedAt != nil && !msg.CreatedAt.IsZero() {
		return
	}
	ts := fallback
	msg.CreatedAt = &ts
}

func normalizeHistoryCreatedAt(history []providers.Message) {
	now := time.Now()
	for i := range history {
		ensureMessageCreatedAt(&history[i], now)
	}
}

func (sm *SessionManager) AddMessage(sessionKey, role, content string) {
	sm.AddFullMessage(sessionKey, providers.Message{
		Role:    role,
		Content: content,
	})
}

// AddFullMessage 向会话中添加一条完整消息，包含工具调用和工具调用 ID。
// 这用于保存包括工具调用与工具结果在内的完整对话流程。
func (sm *SessionManager) AddFullMessage(sessionKey string, msg providers.Message) {
	if messageutil.IsTransientAssistantThoughtMessage(msg) {
		return
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{
			Key:      sessionKey,
			Messages: []providers.Message{},
			Created:  time.Now(),
		}
		sm.sessions[sessionKey] = session
	}

	now := time.Now()
	ensureMessageCreatedAt(&msg, now)

	session.Messages = append(session.Messages, msg)
	session.Updated = now
}

func (sm *SessionManager) GetHistory(key string) []providers.Message {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok {
		return []providers.Message{}
	}

	history := make([]providers.Message, len(session.Messages))
	copy(history, session.Messages)
	return history
}

func (sm *SessionManager) GetSummary(key string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok {
		return ""
	}
	return session.Summary
}

func (sm *SessionManager) SetSummary(key string, summary string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		session.Summary = summary
		session.Updated = time.Now()
	}
}

func (sm *SessionManager) TruncateHistory(key string, keepLast int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if !ok {
		return
	}

	if keepLast <= 0 {
		session.Messages = []providers.Message{}
		session.Updated = time.Now()
		return
	}

	if len(session.Messages) <= keepLast {
		return
	}

	session.Messages = session.Messages[len(session.Messages)-keepLast:]
	session.Updated = time.Now()
}

func (sm *SessionManager) ListSessions() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	keys := make([]string, 0, len(sm.sessions))
	for k := range sm.sessions {
		keys = append(keys, k)
	}
	return keys
}

// sanitizeFilename 将会话键转换为跨平台安全的文件名。
// 把 ':'（会话键分隔符）替换为 '_'，把 '/' 和 '\' 也替换为 '_'，
// 从而避免复合 ID（例如 Telegram 论坛的 "chatID/threadID"）创建
// 子目录或在 Windows 上出错。原始键保留在 JSON 文件内部，
// 因此 loadSessions 仍能映射回正确的内存键。
func sanitizeFilename(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

func (sm *SessionManager) Save(key string) error {
	if sm.storage == "" {
		return nil
	}

	filename := sanitizeFilename(key)

	// filepath.IsLocal 会拒绝空名称、".."、绝对路径以及
	// 操作系统保留的设备名（Windows 上的 NUL、COM1 等）。sanitizeFilename
	// 已将 '/' 和 '\' 替换为 '_'，因此不会创建子目录。
	if filename == "." || !filepath.IsLocal(filename) {
		return os.ErrInvalid
	}

	// 在读锁下做快照，解锁后再执行较慢的文件 I/O。
	sm.mu.RLock()
	stored, ok := sm.sessions[key]
	if !ok {
		sm.mu.RUnlock()
		return nil
	}

	snapshot := Session{
		Key:     stored.Key,
		Summary: stored.Summary,
		Created: stored.Created,
		Updated: stored.Updated,
	}
	if stored.TaskState != nil {
		taskState := stored.TaskState.Clone()
		snapshot.TaskState = &taskState
	}
	if len(stored.Messages) > 0 {
		snapshot.Messages = messageutil.FilterInvalidHistoryMessages(stored.Messages)
	} else {
		snapshot.Messages = []providers.Message{}
	}
	sm.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}

	sessionPath := filepath.Join(sm.storage, filename+".json")
	tmpFile, err := os.CreateTemp(sm.storage, "session-*.tmp")
	if err != nil {
		return err
	}

	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, sessionPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (sm *SessionManager) loadSessions() error {
	files, err := os.ReadDir(sm.storage)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		if filepath.Ext(file.Name()) != ".json" {
			continue
		}

		sessionPath := filepath.Join(sm.storage, file.Name())
		data, err := os.ReadFile(sessionPath)
		if err != nil {
			continue
		}

		var session Session
		if err := json.Unmarshal(data, &session); err != nil {
			continue
		}
		session.Messages = messageutil.FilterInvalidHistoryMessages(session.Messages)
		normalizeHistoryCreatedAt(session.Messages)

		sm.sessions[session.Key] = &session
	}

	return nil
}

// Close 对内存版 SessionManager 是空操作；它满足 SessionStore 接口，
// 以便调用方能够统一释放资源。
func (sm *SessionManager) Close() error {
	return nil
}

// SetHistory 更新会话的消息。
func (sm *SessionManager) SetHistory(key string, history []providers.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		history = messageutil.FilterInvalidHistoryMessages(history)
		// 创建深拷贝，以严格隔离内部状态
		// 与调用方持有的切片。
		msgs := make([]providers.Message, len(history))
		copy(msgs, history)
		normalizeHistoryCreatedAt(msgs)
		session.Messages = msgs
		session.Updated = time.Now()
	}
}
