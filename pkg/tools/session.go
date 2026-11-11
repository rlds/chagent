package tools

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxOutputBufferSize = 1 * 1024 * 1024 // 1MB

const outputTruncateMarker = "\n... [output truncated, exceeded 1MB]\n"

// PtyKeyMode 表示 PTY 会话的方向键编码模式。
// 程序通过发送 smkx/rmkx 序列在 CSI 与 SS3 模式之间切换。
type PtyKeyMode uint8

const (
	PtyKeyModeCSI PtyKeyMode = iota // 由 rmkx (\x1b[?1l) 触发
	PtyKeyModeSS3                   // 由 smkx (\x1b[?1h) 触发
)

const PtyKeyModeNotFound PtyKeyMode = 255

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionDone     = errors.New("session already completed")
	ErrPTYNotSupported = errors.New("PTY is not supported on this platform")
	ErrNoStdin         = errors.New("no stdin available")
)

type ProcessSession struct {
	mu              sync.Mutex
	ID              string
	PID             int
	Command         string
	PTY             bool
	Background      bool
	StartTime       int64
	ExitCode        int
	Status          string
	stdinWriter     io.Writer
	stdoutPipe      io.Reader
	outputBuffer    *bytes.Buffer
	outputTruncated bool
	ptyMaster       *os.File

	// orphanGuardStop 关闭本会话的父进程死亡看护管道。
	orphanGuardStop func()
	orphanGuardOnce sync.Once

	// ptyKeyMode 跟踪方向键编码模式（CSI 还是 SS3）
	ptyKeyMode PtyKeyMode
}

// stopOrphanGuard 释放父进程死亡看护（幂等）。
func (s *ProcessSession) stopOrphanGuard() {
	if s == nil {
		return
	}
	s.orphanGuardOnce.Do(func() {
		if s.orphanGuardStop != nil {
			s.orphanGuardStop()
		}
	})
}

func (s *ProcessSession) IsDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status == "done" || s.Status == "exited"
}

func (s *ProcessSession) GetPtyKeyMode() PtyKeyMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptyKeyMode
}

func (s *ProcessSession) SetPtyKeyMode(mode PtyKeyMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptyKeyMode = mode
}

func (s *ProcessSession) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

func (s *ProcessSession) SetStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

func (s *ProcessSession) GetExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExitCode
}

func (s *ProcessSession) SetExitCode(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExitCode = code
}

func (s *ProcessSession) killProcess() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != "running" {
		return ErrSessionDone
	}

	pid := s.PID
	if pid <= 0 {
		return ErrSessionNotFound
	}

	if err := killProcessGroup(pid); err != nil {
		return err
	}

	s.Status = "done"
	s.ExitCode = -1
	return nil
}

func (s *ProcessSession) Kill() error {
	return s.killProcess()
}

func (s *ProcessSession) Write(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != "running" {
		return ErrSessionDone
	}

	var writer io.Writer
	if s.PTY && s.ptyMaster != nil {
		writer = s.ptyMaster
	} else if s.stdinWriter != nil {
		writer = s.stdinWriter
	} else {
		return ErrNoStdin
	}

	_, err := writer.Write([]byte(data))
	return err
}

func (s *ProcessSession) Read() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.outputBuffer.Len() == 0 {
		return ""
	}

	data := s.outputBuffer.String()
	s.outputBuffer.Reset()
	return data
}

func (s *ProcessSession) ToSessionInfo() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	return SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Status:    s.Status,
		PID:       s.PID,
		StartedAt: s.StartTime,
	}
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ProcessSession
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*ProcessSession),
		stopCh:   make(chan struct{}),
	}

	// 启动清理 goroutine：每 5 分钟运行一次，清理已完成超过 30 分钟的会话
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sm.stopCh:
				return
			case <-ticker.C:
				sm.cleanupOldSessions()
			}
		}
	}()

	return sm
}

// Stop 关闭后台清理 goroutine。从并发 goroutine 中多次调用是安全的。
// Stop 返回后 SessionManager 仍可继续使用——只是清理 goroutine 被终止。
func (sm *SessionManager) Stop() {
	sm.stopOnce.Do(func() {
		close(sm.stopCh)
	})
}

// cleanupOldSessions 删除已完成且超过 30 分钟的会话
func (sm *SessionManager) cleanupOldSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cutoff := time.Now().Add(-30 * time.Minute)
	for id, session := range sm.sessions {
		if session.IsDone() && session.StartTime < cutoff.Unix() {
			delete(sm.sessions, id)
		}
	}
}

func (sm *SessionManager) Add(session *ProcessSession) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = session
}

func (sm *SessionManager) Get(sessionID string) (*ProcessSession, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	return session, nil
}

func (sm *SessionManager) Remove(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}

func (sm *SessionManager) List() []SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]SessionInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		result = append(result, session.ToSessionInfo())
	}

	return result
}

func generateSessionID() string {
	return uuid.New().String()[:8]
}
