package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/messageutil"
	"github.com/rlds/chagent/pkg/taskstate"
)

const (
	// numLockShards 是用于串行化按会话访问的固定互斥锁数量。
	// 使用分片数组而非 map，可以让内存占用不随进程生命周期内
	// 创建的会话数量增长而保持有界 —— 这对长期运行的守护进程
	// 很重要。
	numLockShards = 64

	// maxLineSize 是 .jsonl 文件中单行 JSON 的最大尺寸。
	// 工具结果（read_file、web 搜索等）可能很大，因此设置了
	// 宽松的上限。扫描器从 64 KB 起步，仅在需要时增长到该上限。
	maxLineSize = 10 * 1024 * 1024 // 10 MB
)

// SessionMeta 保存每个会话的元数据，存放在 .meta.json 文件中。
//
// Scope 以原始 JSON 形式存储，这样 pkg/memory 可以与更高层的
// session 包保持解耦，同时仍能保留结构化的 scope 数据。
type SessionMeta struct {
	Key       string          `json:"key"`
	Summary   string          `json:"summary"`
	Skip      int             `json:"skip"`
	Count     int             `json:"count"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Scope     json.RawMessage `json:"scope,omitempty"`
	Aliases   []string        `json:"aliases,omitempty"`
	// TaskState 保存会话的持久化自主任务状态。
	// 较旧的 meta 文件没有 task_state 字段，加载时该值为 nil。
	TaskState *taskstate.State `json:"task_state,omitempty"`
}

// JSONLStore 使用仅追加的 JSONL 文件实现 Store。
//
// 每个会话存储为两个文件：
//
//	{sanitized_key}.jsonl      — 每行一条 JSON 编码的消息，仅追加
//	{sanitized_key}.meta.json  — 会话元数据（摘要、逻辑截断偏移量）
//
// 消息永远不会从 JSONL 文件中被物理删除。相反，
// TruncateHistory 在元数据文件中记录一个 "skip" 偏移量，
// GetHistory 会忽略该偏移量之前的行。这使所有写入都保持
// 仅追加，既快速又具备崩溃安全性。
type JSONLStore struct {
	dir   string
	locks [numLockShards]sync.Mutex
}

// NewJSONLStore 创建一个以 dir 为根目录、基于 JSONL 的存储。
func NewJSONLStore(dir string) (*JSONLStore, error) {
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return nil, fmt.Errorf("memory: create directory: %w", err)
	}
	return &JSONLStore{dir: dir}, nil
}

// sessionLock 返回给定会话键对应的互斥锁。
// 键通过 FNV 哈希映射到固定的分片池，因此无论会话总数
// 多少，内存占用都是 O(1)。
func (s *JSONLStore) sessionLock(key string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &s.locks[h.Sum32()%numLockShards]
}

func (s *JSONLStore) jsonlPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".jsonl")
}

func (s *JSONLStore) metaPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".meta.json")
}

// sanitizeKey 将会话键转换为安全的文件名组成部分。
// 与 pkg/session.sanitizeFilename 保持一致，以便迁移路径匹配。
// 把 ':'（会话键分隔符）替换为 '_'，并把 '/' 和 '\' 也替换为 '_'，
// 这样复合 ID（例如 Telegram 论坛的 "chatID/threadID"、Slack 的
// "channel/thread_ts"）就不会创建子目录或在 Windows 上出错。
func sanitizeKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// readMeta 加载会话的元数据文件。
// 若文件不存在，返回零值 SessionMeta。
func (s *JSONLStore) readMeta(key string) (SessionMeta, error) {
	data, err := os.ReadFile(s.metaPath(key))
	if os.IsNotExist(err) {
		return SessionMeta{Key: key}, nil
	}
	if err != nil {
		return SessionMeta{}, fmt.Errorf("memory: read meta: %w", err)
	}
	var meta SessionMeta
	err = json.Unmarshal(data, &meta)
	if err != nil {
		// 截断/损坏的 meta 文件（崩溃、断电）不能让整个会话
		// 永久不可用：备份它，并根据仅追加的 JSONL 历史重建
		// 一份最小 meta。截断偏移量无法恢复，所以 skip 重置
		// 为 0；原始损坏文件会保留用于取证。此处绝不会重写
		// 消息历史本身。
		healed, healErr := s.healCorruptMeta(key, err)
		if healErr != nil {
			return SessionMeta{}, fmt.Errorf("memory: decode meta: %w (self-heal failed: %v)", err, healErr)
		}
		return healed, nil
	}
	if meta.Key == "" {
		meta.Key = key
	}
	return meta, nil
}

// healCorruptMeta 备份损坏的 meta 文件，并根据 JSONL 行数重建
// 一份最小可用的 SessionMeta。调用它时持有按会话的锁
// （来自 readMeta），因此相对于其他存储操作，重建是原子的。
func (s *JSONLStore) healCorruptMeta(key string, cause error) (SessionMeta, error) {
	metaPath := s.metaPath(key)
	backup := fmt.Sprintf("%s.corrupt-%d", metaPath, time.Now().UnixNano())
	if err := os.Rename(metaPath, backup); err != nil && !os.IsNotExist(err) {
		return SessionMeta{}, fmt.Errorf("backup corrupt meta: %w", err)
	}
	count, lineErr := s.countJSONLLines(key)
	if lineErr != nil {
		return SessionMeta{}, fmt.Errorf("rebuild meta from jsonl: %w", lineErr)
	}
	now := time.Now()
	meta := SessionMeta{
		Key:       key,
		Count:     count,
		Skip:      0,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.writeMeta(key, meta); err != nil {
		return SessionMeta{}, fmt.Errorf("write healed meta: %w", err)
	}
	log.Printf(
		"memory: repaired corrupt session meta key=%q cause=%v backup=%q lines=%d",
		key, cause, backup, count,
	)
	return meta, nil
}

// countJSONLLines 统计会话 JSONL 文件中非空的物理行数。
// 文件不存在时按零行计算。
func (s *JSONLStore) countJSONLLines(key string) (int, error) {
	f, err := os.Open(s.jsonlPath(key))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// writeMeta 使用项目标准的 WriteFileAtomic（临时文件 + fsync + rename）
// 原子地写入元数据文件。
func (s *JSONLStore) writeMeta(key string, meta SessionMeta) error {
	if strings.TrimSpace(meta.Key) == "" {
		meta.Key = key
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode meta: %w", err)
	}
	return fileutil.WriteFileAtomic(s.metaPath(key), data, 0o644)
}

func cloneRawJSON(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

func normalizeAliases(canonicalKey string, aliases []string) []string {
	if len(aliases) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	canonicalKey = strings.TrimSpace(canonicalKey)
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || alias == canonicalKey {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		normalized = append(normalized, alias)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func (s *JSONLStore) sessionExists(key string) bool {
	if key == "" {
		return false
	}
	if _, err := os.Stat(s.jsonlPath(key)); err == nil {
		return true
	}
	if _, err := os.Stat(s.metaPath(key)); err == nil {
		return true
	}
	return false
}

// GetSessionMeta 返回 sessionKey 的当前元数据快照。
func (s *JSONLStore) GetSessionMeta(_ context.Context, sessionKey string) (SessionMeta, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return SessionMeta{}, err
	}
	meta.Scope = cloneRawJSON(meta.Scope)
	if len(meta.Aliases) > 0 {
		meta.Aliases = append([]string(nil), meta.Aliases...)
	}
	if meta.TaskState != nil {
		state := meta.TaskState.Clone()
		meta.TaskState = &state
	}
	return meta, nil
}

// UpsertSessionMeta 存储结构化的会话元数据，同时保留由核心
// JSONL 存储维护的 summary/count/skip 与时间戳。
func (s *JSONLStore) UpsertSessionMeta(
	_ context.Context,
	sessionKey string,
	scope json.RawMessage,
	aliases []string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	meta.Scope = cloneRawJSON(scope)
	meta.Aliases = normalizeAliases(sessionKey, aliases)
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now

	return s.writeMeta(sessionKey, meta)
}

// PromoteAliasHistory 在规范会话仍为空时，原子地把第一个非空
// 别名会话提升为规范会话。
//
// 主会话别名（例如 "agent:main:main" 或其不透明形式）在提升时
// 会被跳过。主会话是共享的全局兜底，把它的历史提升到各个会话
// 会让每条新的 Web UI 会话都附带陈旧消息（issue #2972）。
func (s *JSONLStore) PromoteAliasHistory(
	_ context.Context,
	sessionKey string,
	scope json.RawMessage,
	aliases []string,
) (bool, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false, nil
	}

	aliases = normalizeAliases(sessionKey, aliases)
	for _, alias := range aliases {
		if isMainSessionAlias(alias) {
			continue
		}
		unlock := s.lockSessionPair(sessionKey, alias)
		promoted, err := s.promoteAliasHistoryLocked(sessionKey, alias, scope, aliases)
		unlock()
		if err != nil || promoted {
			return promoted, err
		}
	}

	return false, nil
}

// isMainSessionAlias 判断 alias 是否为旧式或不透明的主会话键。
// 主会话（"agent:<agent>:main"）是共享兜底，不应把它的历史
// 提升到各个按渠道划分的会话中。
func isMainSessionAlias(alias string) bool {
	if alias == "" {
		return false
	}
	// 旧式形式："agent:main:main"（恰好 3 个以冒号分隔的部分）
	// 不能匹配 "agent:sales:direct:main" 等。
	if strings.HasPrefix(alias, "agent:") && strings.HasSuffix(alias, ":main") {
		parts := strings.SplitN(alias, ":", 4)
		if len(parts) == 3 {
			return true
		}
	}
	// 不透明形式："sk_v1_" + SHA256("agent:main:main")
	if strings.HasPrefix(alias, "sk_v1_") {
		for _, agentID := range []string{"main", "Main", "MAIN"} {
			legacy := "agent:" + agentID + ":main"
			hash := sha256.Sum256([]byte(legacy))
			if "sk_v1_"+hex.EncodeToString(hash[:]) == alias {
				return true
			}
		}
	}
	return false
}

// ResolveSessionKey 返回候选键对应的规范会话键。
// 它尽可能走直连规范键的短路路径，否则扫描一次元数据
// 来解析别名或规范元数据键。
func (s *JSONLStore) ResolveSessionKey(_ context.Context, sessionKey string) (string, bool, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "", false, nil
	}

	hasDirectSession := s.sessionExists(sessionKey)
	if hasDirectSession && shouldShortCircuitSessionResolve(sessionKey) {
		return sessionKey, true, nil
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", false, fmt.Errorf("memory: read sessions dir: %w", err)
	}

	var directMetaMatch string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}

		data, readErr := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if readErr != nil {
			log.Printf("memory: skipping unreadable meta %s: %v", entry.Name(), readErr)
			continue
		}

		var meta SessionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			log.Printf("memory: skipping corrupt meta %s: %v", entry.Name(), err)
			continue
		}

		if meta.Key == "" {
			continue
		}

		if meta.Key == sessionKey {
			directMetaMatch = meta.Key
		}

		for _, alias := range meta.Aliases {
			if alias == sessionKey && meta.Key != sessionKey {
				return meta.Key, true, nil
			}
		}
	}

	if directMetaMatch != "" {
		return directMetaMatch, true, nil
	}

	if hasDirectSession {
		return sessionKey, true, nil
	}

	return "", false, nil
}

func shouldShortCircuitSessionResolve(sessionKey string) bool {
	sessionKey = strings.TrimSpace(strings.ToLower(sessionKey))
	if sessionKey == "" {
		return false
	}
	return !strings.ContainsAny(sessionKey, ":/\\")
}

func (s *JSONLStore) lockSessionPair(keyA, keyB string) func() {
	lockA := s.sessionLock(keyA)
	lockB := s.sessionLock(keyB)
	if lockA == lockB {
		lockA.Lock()
		return func() { lockA.Unlock() }
	}
	if keyA <= keyB {
		lockA.Lock()
		lockB.Lock()
		return func() {
			lockB.Unlock()
			lockA.Unlock()
		}
	}
	lockB.Lock()
	lockA.Lock()
	return func() {
		lockA.Unlock()
		lockB.Unlock()
	}
}

func (s *JSONLStore) promoteAliasHistoryLocked(
	sessionKey string,
	alias string,
	scope json.RawMessage,
	aliases []string,
) (bool, error) {
	canonicalMeta, err := s.readMeta(sessionKey)
	if err != nil {
		return false, err
	}
	canonicalHasContent, err := s.sessionHasVisibleContentLocked(sessionKey, canonicalMeta)
	if err != nil {
		return false, err
	}
	if canonicalHasContent {
		return false, nil
	}

	aliasMeta, err := s.readMeta(alias)
	if err != nil {
		return false, err
	}
	aliasHistory, err := readMessages(s.jsonlPath(alias), aliasMeta.Skip)
	if err != nil {
		return false, err
	}
	aliasSummary := strings.TrimSpace(aliasMeta.Summary)
	if len(aliasHistory) == 0 && aliasSummary == "" {
		return false, nil
	}

	previousJSONL, hadPreviousJSONL, err := s.readRawJSONL(sessionKey)
	if err != nil {
		return false, err
	}

	now := time.Now()
	if canonicalMeta.CreatedAt.IsZero() {
		canonicalMeta.CreatedAt = now
	}
	canonicalMeta.Scope = cloneRawJSON(scope)
	canonicalMeta.Aliases = normalizeAliases(sessionKey, aliases)
	canonicalMeta.Skip = 0
	canonicalMeta.Count = len(aliasHistory)
	canonicalMeta.UpdatedAt = now
	if aliasSummary != "" {
		canonicalMeta.Summary = aliasSummary
	}

	if err := s.rewriteJSONL(sessionKey, aliasHistory); err != nil {
		return false, err
	}
	if err := s.writeMeta(sessionKey, canonicalMeta); err != nil {
		if rollbackErr := s.restoreRawJSONL(sessionKey, previousJSONL, hadPreviousJSONL); rollbackErr != nil {
			return false, fmt.Errorf("memory: write promoted meta: %w (rollback jsonl: %v)", err, rollbackErr)
		}
		return false, err
	}
	return true, nil
}

func (s *JSONLStore) sessionHasVisibleContentLocked(sessionKey string, meta SessionMeta) (bool, error) {
	if strings.TrimSpace(meta.Summary) != "" {
		return true, nil
	}
	history, err := readMessages(s.jsonlPath(sessionKey), meta.Skip)
	if err != nil {
		return false, err
	}
	return len(history) > 0, nil
}

func (s *JSONLStore) readRawJSONL(sessionKey string) ([]byte, bool, error) {
	data, err := os.ReadFile(s.jsonlPath(sessionKey))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("memory: read jsonl: %w", err)
	}
	return data, true, nil
}

func (s *JSONLStore) restoreRawJSONL(sessionKey string, data []byte, existed bool) error {
	path := s.jsonlPath(sessionKey)
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: remove jsonl rollback: %w", err)
		}
		return nil
	}
	if err := fileutil.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("memory: restore jsonl rollback: %w", err)
	}
	return nil
}

// readMessages 从 .jsonl 文件读取有效的 JSON 行，跳过前 `skip` 行
// 且不对其反序列化。这样可避免对已被逻辑截断的消息执行
// json.Unmarshal 的开销。
// 末尾格式错误的行（例如因崩溃产生）会被静默跳过。
func readMessages(path string, skip int) ([]providers.Message, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []providers.Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: open jsonl: %w", err)
	}
	defer f.Close()

	var msgs []providers.Message
	scanner := bufio.NewScanner(f)
	// 允许工具结果（read_file、web 搜索等）产生长行。
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	lineNum := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lineNum++
		if lineNum <= skip {
			continue
		}
		var msg providers.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			// 损坏行 —— 很可能是崩溃导致的部分写入。
			// 记录日志让运维知道数据被跳过，但不要
			// 让整个读取失败；这是 JSONL 的标准
			// 恢复模式。
			log.Printf("memory: skipping corrupt line %d in %s: %v",
				lineNum, filepath.Base(path), err)
			continue
		}
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}
		msgs = append(msgs, msg)
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("memory: scan jsonl: %w", scanner.Err())
	}

	if msgs == nil {
		msgs = []providers.Message{}
	}
	return msgs, nil
}

// scanRetainedMessageLines 返回非空原始 JSONL 行的总数，
// 以及经 readMessages 过滤后仍保留的原始行号。
// TruncateHistory 用它针对保留的消息计算 keepLast，
// 同时保留元数据中存储的原始行 skip 偏移量。
func scanRetainedMessageLines(path string) (int, []int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, []int{}, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("memory: open jsonl: %w", err)
	}
	defer f.Close()

	rawCount := 0
	retained := make([]int, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		rawCount++

		var msg providers.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}
		retained = append(retained, rawCount)
	}
	if err := scanner.Err(); err != nil {
		return 0, nil, err
	}
	return rawCount, retained, nil
}

func (s *JSONLStore) AddMessage(
	_ context.Context, sessionKey, role, content string,
) error {
	return s.addMsg(sessionKey, providers.Message{
		Role:    role,
		Content: content,
	})
}

func (s *JSONLStore) AddFullMessage(
	_ context.Context, sessionKey string, msg providers.Message,
) error {
	return s.addMsg(sessionKey, msg)
}

// addMsg 是 AddMessage 与 AddFullMessage 的共用实现。
func (s *JSONLStore) addMsg(sessionKey string, msg providers.Message) error {
	if messageutil.IsTransientAssistantThoughtMessage(msg) {
		return nil
	}

	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	now := time.Now()

	if msg.CreatedAt == nil {
		msg.CreatedAt = &now
	}

	// 将消息追加为单行 JSON。
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("memory: marshal message: %w", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(
		s.jsonlPath(sessionKey),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o644,
	)
	if err != nil {
		return fmt.Errorf("memory: open jsonl for append: %w", err)
	}
	_, writeErr := f.Write(line)
	if writeErr != nil {
		f.Close()
		return fmt.Errorf("memory: append message: %w", writeErr)
	}
	// 关闭前刷新到物理存储。这与 writeMeta 和 rewriteJSONL
	// （它们使用带 fsync 的 WriteFileAtomic）的持久性保证一致。
	// 若不调用 Sync，断电可能让追加内容只留在内核页缓存中 ——
	// 重启后就会丢失。
	if syncErr := f.Sync(); syncErr != nil {
		f.Close()
		return fmt.Errorf("memory: sync jsonl: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		return fmt.Errorf("memory: close jsonl: %w", closeErr)
	}

	// 更新元数据。
	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if meta.Count == 0 && meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Count++
	meta.UpdatedAt = now

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) GetHistory(
	_ context.Context, sessionKey string,
) ([]providers.Message, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return nil, err
	}

	// 传入 meta.Skip，让 readMessages 跳过这些行而不反序列化
	// 它们 —— 避免在被截断的消息上浪费 CPU。
	msgs, err := readMessages(s.jsonlPath(sessionKey), meta.Skip)
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

func (s *JSONLStore) GetSummary(
	_ context.Context, sessionKey string,
) (string, error) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return "", err
	}
	return meta.Summary, nil
}

func (s *JSONLStore) SetSummary(
	_ context.Context, sessionKey, summary string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Summary = summary
	meta.UpdatedAt = now

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) TruncateHistory(
	_ context.Context, sessionKey string, keepLast int,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}

	rawCount, retainedRawLines, scanErr := scanRetainedMessageLines(s.jsonlPath(sessionKey))
	if scanErr != nil {
		return scanErr
	}
	meta.Count = rawCount
	if meta.Skip > meta.Count {
		meta.Skip = meta.Count
	}

	activeStart := sort.Search(len(retainedRawLines), func(i int) bool {
		return retainedRawLines[i] > meta.Skip
	})
	activeRetainedCount := len(retainedRawLines) - activeStart

	switch {
	case keepLast <= 0 || activeRetainedCount == 0:
		meta.Skip = meta.Count
	case keepLast < activeRetainedCount:
		activeRawLines := retainedRawLines[activeStart:]
		meta.Skip = activeRawLines[activeRetainedCount-keepLast-1]
	}
	meta.UpdatedAt = time.Now()

	return s.writeMeta(sessionKey, meta)
}

func (s *JSONLStore) SetHistory(
	_ context.Context,
	sessionKey string,
	history []providers.Message,
) error {
	history = messageutil.FilterInvalidHistoryMessages(history)

	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	now := time.Now()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Skip = 0
	meta.Count = len(history)
	meta.UpdatedAt = now

	for i := range history {
		if history[i].CreatedAt == nil {
			history[i].CreatedAt = &now
		}
	}

	// 在重写 JSONL 文件之前先写入 meta。如果两次写入之间崩溃，
	// meta 的 Skip=0 而旧文件仍然完整，因此 GetHistory 会从
	// 第 1 行开始读取 —— 返回"过多"消息而非丢失数据。
	// 下一次 SetHistory 调用会修正这一点。
	err = s.writeMeta(sessionKey, meta)
	if err != nil {
		return err
	}

	return s.rewriteJSONL(sessionKey, history)
}

// Compact 物理重写 JSONL 文件，丢弃所有已被逻辑跳过的行。
// 这会回收反复调用 TruncateHistory 后累积的磁盘空间。
//
// 任何时候调用都是安全的；若没有可压缩的内容
// （skip == 0），方法会立即返回。
func (s *JSONLStore) Compact(
	_ context.Context, sessionKey string,
) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if meta.Skip == 0 {
		return nil
	}

	// 只读取活跃消息，跳过被截断的行而不反序列化
	// 它们。
	active, err := readMessages(s.jsonlPath(sessionKey), meta.Skip)
	if err != nil {
		return err
	}

	// 在重写 JSONL 文件之前先写入 meta。如果进程在两次写入之间
	// 崩溃，meta 的 Skip=0 而旧的（未压缩）文件仍然完整，因此
	// GetHistory 会从第 1 行开始读取 —— 返回此前被截断的消息
	// 而非丢失数据。下一次 Compact 或 TruncateHistory 会修正这一点。
	meta.Skip = 0
	meta.Count = len(active)
	meta.UpdatedAt = time.Now()

	err = s.writeMeta(sessionKey, meta)
	if err != nil {
		return err
	}

	return s.rewriteJSONL(sessionKey, active)
}

// rewriteJSONL 使用项目标准的 WriteFileAtomic（临时文件 + fsync + rename）
// 原子地用给定消息替换 JSONL 文件。
func (s *JSONLStore) rewriteJSONL(
	sessionKey string, msgs []providers.Message,
) error {
	msgs = messageutil.FilterInvalidHistoryMessages(msgs)

	var buf bytes.Buffer
	for i, msg := range msgs {
		line, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("memory: marshal message %d: %w", i, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return fileutil.WriteFileAtomic(s.jsonlPath(sessionKey), buf.Bytes(), 0o644)
}

// ListSessions 通过读取 .meta.json 文件返回所有已知的会话键。
func (s *JSONLStore) ListSessions() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var keys []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		// 读取 meta 文件以获取原始键
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		var meta SessionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if meta.Key != "" {
			keys = append(keys, meta.Key)
		}
	}
	return keys
}

func (s *JSONLStore) Close() error {
	return nil
}
