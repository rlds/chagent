package media

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rlds/chagent/pkg/logger"
)

// CleanupPolicy 控制 MediaStore 在 ref 被释放或过期时如何处理底层文件。
type CleanupPolicy string

const (
	// CleanupPolicyDeleteOnCleanup 表示该文件由存储管理，在指向该路径的最后一个
	// ref 消失后可以被删除。
	CleanupPolicyDeleteOnCleanup CleanupPolicy = "delete_on_cleanup"
	// CleanupPolicyForgetOnly 表示存储只应删除 ref 映射，绝不能删除底层文件。
	CleanupPolicyForgetOnly CleanupPolicy = "forget_only"
)

// MediaMeta 保存已存储媒体文件的元数据。
type MediaMeta struct {
	Filename      string
	ContentType   string
	Source        string        // "telegram"、"discord"、"tool:image-gen" 等。
	CleanupPolicy CleanupPolicy // 默认值为 CleanupPolicyDeleteOnCleanup
}

// MediaStore 管理关联到处理作用域的媒体文件生命周期。
type MediaStore interface {
	// Store 在给定作用域下注册一个已存在的本地文件。
	// 返回一个 ref 标识符（例如 "media://<id>"）。
	// Store 不会移动或复制该文件，只记录映射关系。
	// 如果 meta.CleanupPolicy 为空，则默认使用 CleanupPolicyDeleteOnCleanup。
	Store(localPath string, meta MediaMeta, scope string) (ref string, err error)

	// Resolve 返回给定 ref 对应的本地文件路径。
	Resolve(ref string) (localPath string, err error)

	// ResolveWithMeta 返回给定 ref 对应的本地文件路径和元数据。
	ResolveWithMeta(ref string) (localPath string, meta MediaMeta, err error)

	// ReleaseAll 删除给定作用域下注册的所有文件
	// 并移除映射条目。文件不存在的错误会被忽略。
	ReleaseAll(scope string) error
}

// mediaEntry 保存已存储媒体文件的路径和元数据。
type mediaEntry struct {
	path     string
	meta     MediaMeta
	storedAt time.Time
}

type pathRefState struct {
	refCount       int
	deleteEligible bool
}

// MediaCleanerConfig 配置后台 TTL 清理。
type MediaCleanerConfig struct {
	Enabled  bool
	MaxAge   time.Duration
	Interval time.Duration
}

// FileMediaStore 是 MediaStore 的纯内存实现。
// 文件应已存在于磁盘上（例如位于 /tmp/chagent_media/）。
type FileMediaStore struct {
	mu          sync.RWMutex
	refs        map[string]mediaEntry
	scopeToRefs map[string]map[string]struct{}
	refToScope  map[string]string
	refToPath   map[string]string
	pathStates  map[string]pathRefState

	cleanerCfg MediaCleanerConfig
	stop       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	nowFunc    func() time.Time // 用于测试
}

// NewFileMediaStore 创建一个不带后台清理的 FileMediaStore。
func NewFileMediaStore() *FileMediaStore {
	return &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		refToPath:   make(map[string]string),
		pathStates:  make(map[string]pathRefState),
		nowFunc:     time.Now,
	}
}

// NewFileMediaStoreWithCleanup 创建一个带基于 TTL 的后台清理的 FileMediaStore。
func NewFileMediaStoreWithCleanup(cfg MediaCleanerConfig) *FileMediaStore {
	return &FileMediaStore{
		refs:        make(map[string]mediaEntry),
		scopeToRefs: make(map[string]map[string]struct{}),
		refToScope:  make(map[string]string),
		refToPath:   make(map[string]string),
		pathStates:  make(map[string]pathRefState),
		cleanerCfg:  cfg,
		stop:        make(chan struct{}),
		nowFunc:     time.Now,
	}
}

// Store 在给定作用域下注册一个本地文件。该文件必须存在。
func (s *FileMediaStore) Store(localPath string, meta MediaMeta, scope string) (string, error) {
	if _, err := os.Stat(localPath); err != nil {
		return "", fmt.Errorf("media store: %s: %w", localPath, err)
	}

	ref := "media://" + uuid.New().String()
	meta.CleanupPolicy = normalizeCleanupPolicy(meta.CleanupPolicy)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.refs[ref] = mediaEntry{path: localPath, meta: meta, storedAt: s.nowFunc()}
	if s.scopeToRefs[scope] == nil {
		s.scopeToRefs[scope] = make(map[string]struct{})
	}
	s.scopeToRefs[scope][ref] = struct{}{}
	s.refToScope[ref] = scope
	s.refToPath[ref] = localPath

	pathState := s.pathStates[localPath]
	if pathState.refCount == 0 {
		pathState.deleteEligible = meta.CleanupPolicy == CleanupPolicyDeleteOnCleanup
	} else if meta.CleanupPolicy == CleanupPolicyForgetOnly {
		// 保守起见：一旦某个路径被外部借用，即使同时存在存储管理的 ref，
		// 也绝不允许此生命周期自动删除它。
		pathState.deleteEligible = false
	}
	pathState.refCount++
	s.pathStates[localPath] = pathState

	return ref, nil
}

// Resolve 返回给定 ref 对应的本地路径。
func (s *FileMediaStore) Resolve(ref string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.refs[ref]
	if !ok {
		return "", fmt.Errorf("media store: unknown ref: %s", ref)
	}
	return entry.path, nil
}

// ResolveWithMeta 返回给定 ref 对应的本地路径和元数据。
func (s *FileMediaStore) ResolveWithMeta(ref string) (string, MediaMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.refs[ref]
	if !ok {
		return "", MediaMeta{}, fmt.Errorf("media store: unknown ref: %s", ref)
	}
	return entry.path, entry.meta, nil
}

// ReleaseAll 删除给定作用域下的所有文件，并清理映射关系。
// 阶段 1（持锁）：从映射表中移除条目。
// 阶段 2（不持锁）：在路径的最后一个 ref 消失后，从磁盘删除由存储管理的文件。
func (s *FileMediaStore) ReleaseAll(scope string) error {
	// 阶段 1：持锁收集路径并从映射表中移除
	var paths []string

	s.mu.Lock()
	refs, ok := s.scopeToRefs[scope]
	if !ok {
		s.mu.Unlock()
		return nil
	}

	for ref := range refs {
		fallbackPath := ""
		if entry, exists := s.refs[ref]; exists {
			fallbackPath = entry.path
		}
		if removablePath, shouldDelete := s.releaseRefLocked(ref, fallbackPath); shouldDelete {
			paths = append(paths, removablePath)
		}
	}
	delete(s.scopeToRefs, scope)
	s.mu.Unlock()

	// 阶段 2：不持锁删除文件
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "release: failed to remove file", map[string]any{
				"path":  p,
				"error": err.Error(),
			})
		}
	}

	return nil
}

// CleanExpired 移除所有早于 MaxAge 的条目。
// 阶段 1（持锁）：识别过期条目并从映射表中移除。
// 阶段 2（不持锁）：从磁盘删除由存储管理的文件，以尽量减少锁争用。
func (s *FileMediaStore) CleanExpired() int {
	if s.cleanerCfg.MaxAge <= 0 {
		return 0
	}

	// 阶段 1：持锁收集过期条目
	type expiredEntry struct {
		ref        string
		deletePath string
	}

	s.mu.Lock()
	cutoff := s.nowFunc().Add(-s.cleanerCfg.MaxAge)
	var expired []expiredEntry

	for ref, entry := range s.refs {
		if entry.storedAt.Before(cutoff) {
			if scope, ok := s.refToScope[ref]; ok {
				if scopeRefs, ok := s.scopeToRefs[scope]; ok {
					delete(scopeRefs, ref)
					if len(scopeRefs) == 0 {
						delete(s.scopeToRefs, scope)
					}
				}
			}

			expiredItem := expiredEntry{ref: ref}
			if deletePath, shouldDelete := s.releaseRefLocked(ref, entry.path); shouldDelete {
				expiredItem.deletePath = deletePath
			}
			expired = append(expired, expiredItem)
		}
	}
	s.mu.Unlock()

	// 阶段 2：不持锁删除文件
	for _, e := range expired {
		if e.deletePath == "" {
			continue
		}
		if err := os.Remove(e.deletePath); err != nil && !os.IsNotExist(err) {
			logger.WarnCF("media", "cleanup: failed to remove file", map[string]any{
				"path":  e.deletePath,
				"error": err.Error(),
			})
		}
	}

	return len(expired)
}

func normalizeCleanupPolicy(policy CleanupPolicy) CleanupPolicy {
	switch policy {
	case "", CleanupPolicyDeleteOnCleanup:
		return CleanupPolicyDeleteOnCleanup
	case CleanupPolicyForgetOnly:
		return CleanupPolicyForgetOnly
	default:
		return CleanupPolicyDeleteOnCleanup
	}
}

func (s *FileMediaStore) releaseRefLocked(ref, fallbackPath string) (string, bool) {
	path := fallbackPath
	if storedPath, ok := s.refToPath[ref]; ok {
		path = storedPath
		delete(s.refToPath, ref)
	}

	delete(s.refs, ref)
	delete(s.refToScope, ref)

	if path == "" {
		return "", false
	}

	pathState, ok := s.pathStates[path]
	if !ok {
		return "", false
	}
	if pathState.refCount <= 1 {
		delete(s.pathStates, path)
		return path, pathState.deleteEligible
	}

	pathState.refCount--
	s.pathStates[path] = pathState
	return "", false
}

// Start 在启用清理时启动后台清理 goroutine。
// 可以安全地多次调用；只有第一次调用会启动该 goroutine。
func (s *FileMediaStore) Start() {
	if !s.cleanerCfg.Enabled || s.stop == nil {
		return
	}
	if s.cleanerCfg.Interval <= 0 || s.cleanerCfg.MaxAge <= 0 {
		logger.WarnCF("media", "cleanup: skipped due to invalid config", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})
		return
	}

	s.startOnce.Do(func() {
		logger.InfoCF("media", "cleanup enabled", map[string]any{
			"interval": s.cleanerCfg.Interval.String(),
			"max_age":  s.cleanerCfg.MaxAge.String(),
		})

		go func() {
			ticker := time.NewTicker(s.cleanerCfg.Interval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if n := s.CleanExpired(); n > 0 {
						logger.InfoCF("media", "cleanup: removed expired entries", map[string]any{
							"count": n,
						})
					}
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop 终止后台清理 goroutine。
// 可以安全地多次调用；只有第一次调用会关闭该通道。
func (s *FileMediaStore) Stop() {
	if s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}
