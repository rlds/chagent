// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/logger"
)

const (
	// defaultSpillMaxBytes 把单条 spill 条目限制在 8 MiB。超出上限的内容会用
	// spillTruncationSuffix 截断；模型可见通知仍然准确，因为它说明存储副本已截断。
	defaultSpillMaxBytes = 8 << 20
	// defaultSpillMaxAge 是 spill 条目的保留时长（30 天），
	// 与 DSH spill-local 的 cleanupPeriodDays 默认值一致。
	defaultSpillMaxAge = 30 * 24 * time.Hour
	// spillCleanupInterval 限制 Save 执行的顺带清理频率。
	spillCleanupInterval = time.Hour
	// spillTruncationSuffix 标记达到 maxBytes 的存储副本。
	spillTruncationSuffix = "\n... [spill truncated]"
	// spillDirName 是每个工作区存放 spill 输出的目录。
	spillDirName = "spill"
)

// SpillStore 把超大的模型可见文本持久化到私有工作区目录，并返回模型之后可读取的定位符。
//
// 它对应 DSH 的 spill-local 后端（packages/spill/spill-local）：该接缝只负责持久性
// （路径安全、权限、保留期）；决定溢出什么以及如何渲染省略通知的逻辑在 pruner 中。
//
// 零值不可用；请用 NewSpillStore 构造。nil *SpillStore 是合法的，表示“禁用溢出”，
// 此时所有方法都会安全降级。
type SpillStore struct {
	dir      string
	maxBytes int
	maxAge   time.Duration
}

// spillCleanupThrottle 记录每个存储目录最近一次顺带清理的时间。
// 它放在包级是因为调用方可能每次压缩都新建 SpillStore；
// 保留期扫描仍最多每目录每小时运行一次。
var spillCleanupThrottle = struct {
	mu    sync.Mutex
	byDir map[string]time.Time
}{byDir: make(map[string]time.Time)}

// NewSpillStore 返回根目录为 <workspace>/.chagent/spill 的存储；
// 未配置可用工作区时返回 nil（此时禁用溢出，调用方回退到之前的 inline 行为）。
func NewSpillStore(workspace string) *SpillStore {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		logger.WarnCF("agent", "spill disabled: cannot resolve workspace", map[string]any{
			"workspace": workspace,
			"error":     err.Error(),
		})
		return nil
	}
	return &SpillStore{
		dir:      filepath.Join(abs, ".chagent", spillDirName),
		maxBytes: defaultSpillMaxBytes,
		maxAge:   defaultSpillMaxAge,
	}
}

// Dir 返回存储的绝对根目录；nil 存储返回 ""。
func (s *SpillStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Save 把内容写到 <dir>/<session>/<label>-<sha256(content)[:16]>.txt 并返回绝对路径。
//
// 路径安全：sessionKey 与 label 被编码为单个防穿越的路径段（仅保留 [A-Za-z0-9._-]），
// 因此受攻击者影响的会话键或工具调用 id 永远无法逃出存储目录。目录以 0700 创建，
// 文件以 0600 原子写入（临时文件 + rename）。
//
// 文件名按内容寻址，因此保存相同内容两次是幂等的，且绝不会覆盖不同的负载。
func (s *SpillStore) Save(sessionKey, label, content string) (path string, err error) {
	if s == nil {
		return "", fmt.Errorf("spill: store is nil")
	}

	path = s.pathFor(sessionKey, label, content)
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return "", fmt.Errorf("spill: create directory: %w", mkErr)
	}

	// 按内容寻址的名称：已存在的普通文件已持有完全相同的内容，因此无需重写。
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
		s.maybeCleanup()
		return path, nil
	}

	payload := s.truncate(content)
	if writeErr := writeSpillFileAtomic(path, []byte(payload), 0o600); writeErr != nil {
		return "", fmt.Errorf("spill: write %s: %w", path, writeErr)
	}

	s.maybeCleanup()
	return path, nil
}

// pathFor 是 Save 为该 (sessionKey, label, content) 写入的确定性、内容寻址路径；
// 它从不访问文件系统。需要在决定溢出前估算定位符大小的调用方可以使用它，
// 从而完全跳过失败或无意义的保存。
func (s *SpillStore) pathFor(sessionKey, label, content string) string {
	if s == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	name := sanitizeSpillSegment(label) + "-" + hex.EncodeToString(sum[:])[:16] + ".txt"
	return filepath.Join(s.dir, sanitizeSpillSegment(sessionKey), name)
}

// Cleanup 删除存储下 mtime 早于 maxAge 的普通文件，然后清理空的会话目录。
// 绝不跟随或删除符号链接。存储目录不存在不是错误。
func (s *SpillStore) Cleanup() (removed int, err error) {
	if s == nil || s.maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-s.maxAge)

	var sessionDirs []string
	walkErr := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if path == s.dir {
			if !d.IsDir() {
				// 存储根意外地是一个文件；绝不要删除它。
				return nil
			}
			return nil
		}
		if d.IsDir() {
			sessionDirs = append(sessionDirs, path)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// 绝不跟随或删除符号链接：它们可能指向存储之外。
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			logger.WarnCF("agent", "spill cleanup: cannot stat entry", map[string]any{
				"path":  path,
				"error": infoErr.Error(),
			})
			return nil
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil {
			logger.WarnCF("agent", "spill cleanup: cannot remove entry", map[string]any{
				"path":  path,
				"error": rmErr.Error(),
			})
			return nil
		}
		removed++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		err = walkErr
	}

	// 按深度优先，使嵌套空目录一次清理完成。
	for i := len(sessionDirs) - 1; i >= 0; i-- {
		_ = os.Remove(sessionDirs[i]) // 非空时会（无害地）失败
	}
	return removed, err
}

// truncate 返回在 UTF-8 边界上截到 maxBytes 的内容，并追加截断后缀，
// 让读者知道存储副本不完整。
func (s *SpillStore) truncate(content string) string {
	if s.maxBytes <= 0 || len(content) <= s.maxBytes {
		return content
	}
	cut := s.maxBytes
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + spillTruncationSuffix
}

// maybeCleanup 每个 spillCleanupInterval 最多运行一次 Cleanup。
// 清理失败会记录并吞掉：绝不能因为保留期维护失败而让溢出操作失败。
func (s *SpillStore) maybeCleanup() {
	if s == nil {
		return
	}
	now := time.Now()

	spillCleanupThrottle.mu.Lock()
	last := spillCleanupThrottle.byDir[s.dir]
	if !last.IsZero() && now.Sub(last) < spillCleanupInterval {
		spillCleanupThrottle.mu.Unlock()
		return
	}
	spillCleanupThrottle.byDir[s.dir] = now
	spillCleanupThrottle.mu.Unlock()

	removed, err := s.Cleanup()
	if err != nil {
		logger.WarnCF("agent", "spill cleanup failed", map[string]any{
			"dir":   s.dir,
			"error": err.Error(),
		})
		return
	}
	if removed > 0 {
		logger.DebugCF("agent", "spill cleanup removed expired entries", map[string]any{
			"dir":     s.dir,
			"removed": removed,
		})
	}
}

// writeSpillFileAtomic 通过同目录临时文件加原子 rename 把数据写入 path，
// 并以 perm 创建文件。
func writeSpillFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".tmp-spill-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmpFile.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// sanitizeSpillSegment 把任意字符串映射为单个安全路径段：
// 只保留 [A-Za-z0-9._-]，其余字符变为 "_"。可能成为 "." 或 ".." 的段
// 会被额外中和，因此没有路径分量可以穿越。
func sanitizeSpillSegment(raw string) string {
	if raw == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	// 仅由点组成的段（"."、".."、"..."）绝不适合作为路径分量；
	// 给它加 "_" 前缀并把点变成 "_"。
	if strings.Trim(out, ".") == "" {
		return "_" + strings.ReplaceAll(out, ".", "_")
	}
	return out
}
