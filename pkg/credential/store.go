package credential

import "sync/atomic"

// SecureStore 在内存中保存口令。
//
// 使用 atomic.Pointer，因此读写无需加锁。
// 口令永远不会写入磁盘；如何在本存储之外传递口令
// （例如通过 cmd.Env 或 os.Environ）由调用方决定。
type SecureStore struct {
	val atomic.Pointer[string]
}

// NewSecureStore 创建一个空的 SecureStore。
func NewSecureStore() *SecureStore {
	return &SecureStore{}
}

// SetString 保存口令。空字符串会清空存储。
func (s *SecureStore) SetString(passphrase string) {
	if passphrase == "" {
		s.val.Store(nil)
		return
	}
	s.val.Store(&passphrase)
}

// Get 返回已存储的口令，未设置时返回 ""。
func (s *SecureStore) Get() string {
	if p := s.val.Load(); p != nil {
		return *p
	}
	return ""
}

// IsSet 报告当前是否已存储口令。
func (s *SecureStore) IsSet() bool {
	return s.val.Load() != nil
}

// Clear 移除已存储的口令。
func (s *SecureStore) Clear() {
	s.val.Store(nil)
}
