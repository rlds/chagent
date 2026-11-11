// Package goal 实现 Chagent 的 DSH goal 工具族：持久化、
// 以工作区为作用域的 goal，并提供 create/get/update 控制。
//
// 该实现是用 Go 重写的 DSH `packages/goal/tool-goal` 中面向模型的部分，
// 以及 `packages/goal/goal` 中的持久化 goal 领域逻辑，范围收窄为
// chess 所需的三个工具：
//
//	create_goal{objective, note?}
//	get_goal{id?, limit?}
//	update_goal{id, status?, note?, evidence?}
//
// Goal 存放于 <workspace>/state/goals/<goal_id>.json，从而能跨轮次、
// 进程重启和会话压缩存活。写入是原子的（临时文件 + fsync + rename），
// 目录权限为 0700，文件权限为 0600。只会触及配置的工作区。
package goal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Goal 状态。状态字符串会原样持久化到 JSON 文件中，
// 并属于面向模型的契约，因此这些常量必须保持稳定。
const (
	StatusActive   = "active"
	StatusComplete = "complete"
	StatusBlocked  = "blocked"
)

// ListLimit 是一次列表读取返回的最大 goal 数量。
const ListLimit = 50

const (
	goalsDirName   = "goals"
	stateDirName   = "state"
	goalFileSuffix = ".json"
)

// idPattern 是持久化 goal id 的形状：单调递增的序号加随机后缀，
// 两者共同保证 id 稳定且不会冲突。
var idPattern = regexp.MustCompile(`^goal-([0-9]{6,})-[0-9a-f]{8}$`)

var validStatuses = map[string]struct{}{
	StatusActive:   {},
	StatusComplete: {},
	StatusBlocked:  {},
}

// Error 是可诊断的 goal 错误。Code 是稳定的机器可读标记
// （例如 "goal_invalid_transition"），Message 说明失败原因以及
// 如何修正调用。工具会把两者都呈现给模型。
type Error struct {
	Code    string
	Message string
}

// Error 实现 error 接口。
func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode 返回 goal Error 的稳定错误码；其他错误返回 ""。
func ErrorCode(err error) string {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

// Goal 是持久化在 <workspace>/state/goals/ 下的一个 goal。
type Goal struct {
	ID        string   `json:"id"`
	Objective string   `json:"objective"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Evidence  []string `json:"evidence"`
	Note      string   `json:"note,omitempty"`
}

// Summary 是一个 goal 的精简列表投影。
type Summary struct {
	ID            string `json:"id"`
	Objective     string `json:"objective"`
	Status        string `json:"status"`
	UpdatedAt     string `json:"updated_at"`
	EvidenceCount int    `json:"evidence_count"`
	Note          string `json:"note,omitempty"`
}

// SkippedFile 记录列举过程中遇到的一个不可读/损坏的 goal 文件。
// 列举不会因为单个坏文件而失败：由调用方报告这些诊断信息。
type SkippedFile struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Manager 持有以工作区为作用域的 goal 存储。它可安全并发使用；
// 所有读写由同一互斥锁串行化，且每次写入都是原子的。
type Manager struct {
	workspace string
	dir       string
	initErr   error

	mu  sync.Mutex
	seq int64
	now func() time.Time
}

// NewManager 返回以 <workspace>/state/goals 为根目录的 goal 管理器。
//
// 工作区会被解析为绝对路径，且必须是非空的目录目标；构造函数接受
// 空/不存在的工作区，但所有操作都会返回可诊断的错误，而不会写到
// 工作区之外。
func NewManager(workspace string) *Manager {
	m := &Manager{now: time.Now}
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		m.initErr = newError("goal_no_workspace", "goal 管理器需要一个非空工作区")
		return m
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		m.initErr = newError("goal_bad_workspace", "解析工作区 %q 失败：%v", trimmed, err)
		return m
	}
	abs = filepath.Clean(abs)
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		m.initErr = newError("goal_bad_workspace", "工作区 %q 不是目录", abs)
		return m
	}
	m.workspace = abs
	m.dir = filepath.Join(abs, stateDirName, goalsDirName)
	m.seq = m.scanMaxSeq()
	return m
}

// Workspace 返回绝对工作区根目录；无效时返回 ""。
func (m *Manager) Workspace() string {
	if m == nil {
		return ""
	}
	return m.workspace
}

// Dir 返回 goals 目录的绝对路径；无效时返回 ""。
func (m *Manager) Dir() string {
	if m == nil {
		return ""
	}
	return m.dir
}

// ready 校验管理器并确保 goals 目录存在，且权限为私有的 0700。
// 调用方需持有 m.mu。
func (m *Manager) ready() error {
	if m == nil {
		return newError("goal_no_manager", "goal 管理器未初始化")
	}
	if m.initErr != nil {
		return m.initErr
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return newError("goal_store_unavailable", "创建 goals 目录失败：%v", err)
	}
	if err := os.Chmod(m.dir, 0o700); err != nil {
		return newError("goal_store_unavailable", "设置 goals 目录权限失败：%v", err)
	}
	return nil
}

// scanMaxSeq 读取一次已有的 goal id，使新 id 在重启后仍能延续序号。
// 不可读的条目在此忽略（由 LoadAll 报告）。
func (m *Manager) scanMaxSeq() int64 {
	if m.dir == "" {
		return 0
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0
	}
	var maxSeq int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), goalFileSuffix) {
			continue
		}
		if seq, ok := parseSeqFromID(strings.TrimSuffix(entry.Name(), goalFileSuffix)); ok && seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq
}

// parseSeqFromID 从 goal id 中提取序号部分。
func parseSeqFromID(id string) (int64, bool) {
	match := idPattern.FindStringSubmatch(id)
	if match == nil {
		return 0, false
	}
	seq, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// goalPath 将 goal id 映射为 goals 目录下的文件路径，
// 并拒绝任何可能逃逸出该目录的 id。
func (m *Manager) goalPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !idPattern.MatchString(id) {
		return "", newError("goal_invalid_id", "无效的 goal id %q（期望格式 goal-<seq>-<8 hex>）", id)
	}
	path := filepath.Join(m.dir, id+goalFileSuffix)
	rel, err := filepath.Rel(m.dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", newError("goal_invalid_id", "goal id %q 越出了 goals 目录", id)
	}
	return path, nil
}

// load 读取并校验一个 goal 文件。调用方需持有 m.mu。
func (m *Manager) load(id string) (Goal, error) {
	path, err := m.goalPath(id)
	if err != nil {
		return Goal{}, err
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return Goal{}, newError("goal_not_found", "goal %q 不存在", id)
		}
		return Goal{}, newError("goal_store_unavailable", "读取 goal %q 失败：%v", id, readErr)
	}
	var goal Goal
	if err := json.Unmarshal(data, &goal); err != nil {
		return Goal{}, newError("goal_corrupt", "goal 文件 %q 不是合法 JSON：%v", filepath.Base(path), err)
	}
	if err := validateGoal(goal, id); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

// validateGoal 校验单个 goal 的持久化不变量。
func validateGoal(goal Goal, wantID string) error {
	if wantID != "" && goal.ID != wantID {
		return newError("goal_corrupt", "goal id %q 与文件名中的 id %q 不一致", goal.ID, wantID)
	}
	if !idPattern.MatchString(goal.ID) {
		return newError("goal_corrupt", "goal id %q 格式非法", goal.ID)
	}
	if strings.TrimSpace(goal.Objective) == "" {
		return newError("goal_corrupt", "goal %q 的 objective 为空", goal.ID)
	}
	if _, ok := validStatuses[goal.Status]; !ok {
		return newError("goal_corrupt", "goal %q 的状态 %q 非法", goal.ID, goal.Status)
	}
	if _, err := time.Parse(time.RFC3339Nano, goal.CreatedAt); err != nil {
		return newError("goal_corrupt", "goal %q 的 created_at 非法：%v", goal.ID, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, goal.UpdatedAt); err != nil {
		return newError("goal_corrupt", "goal %q 的 updated_at 非法：%v", goal.ID, err)
	}
	return nil
}

// write 原子地持久化一个 goal：在同一目录中写入一个 0600 的临时文件，
// 执行 fsync 后重命名覆盖目标文件。调用方需持有 m.mu。
func (m *Manager) write(goal Goal) error {
	if err := m.ready(); err != nil {
		return err
	}
	path, err := m.goalPath(goal.ID)
	if err != nil {
		return err
	}
	if goal.Evidence == nil {
		goal.Evidence = []string{}
	}
	data, err := json.MarshalIndent(goal, "", "  ")
	if err != nil {
		return newError("goal_store_unavailable", "编码 goal %q 失败：%v", goal.ID, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(m.dir, ".goal-*.tmp")
	if err != nil {
		return newError("goal_store_unavailable", "创建临时 goal 文件失败：%v", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return newError("goal_store_unavailable", "设置临时 goal 文件权限失败：%v", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return newError("goal_store_unavailable", "写入临时 goal 文件失败：%v", err)
	}
	if err := tmp.Sync(); err != nil {
		return newError("goal_store_unavailable", "同步临时 goal 文件失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		return newError("goal_store_unavailable", "关闭临时 goal 文件失败：%v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return newError("goal_store_unavailable", "将 goal 文件重命名到目标位置失败：%v", err)
	}
	cleanup = false
	if dir, dirErr := os.Open(m.dir); dirErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// newID 分配下一个稳定的 goal id。调用方需持有 m.mu。
func (m *Manager) newID() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", newError("goal_id_unavailable", "生成随机 goal 后缀失败：%v", err)
	}
	for {
		m.seq++
		id := fmt.Sprintf("goal-%06d-%s", m.seq, hex.EncodeToString(suffix))
		if _, err := os.Lstat(filepath.Join(m.dir, id+goalFileSuffix)); os.IsNotExist(err) {
			return id, nil
		}
	}
}

// Create 持久化一个新的 active goal 并返回它。
func (m *Manager) Create(objective, note string) (Goal, error) {
	if m == nil {
		return Goal{}, newError("goal_no_manager", "goal 管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(); err != nil {
		return Goal{}, err
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return Goal{}, newError("goal_invalid_argument", "objective 必须是非空字符串")
	}
	id, err := m.newID()
	if err != nil {
		return Goal{}, err
	}
	now := m.now().UTC().Format(time.RFC3339Nano)
	goal := Goal{
		ID:        id,
		Objective: objective,
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
		Evidence:  []string{},
		Note:      strings.TrimSpace(note),
	}
	if err := m.write(goal); err != nil {
		return Goal{}, err
	}
	return cloneGoal(goal), nil
}

// Get 按 id 返回一个 goal。
func (m *Manager) Get(id string) (Goal, error) {
	if m == nil {
		return Goal{}, newError("goal_no_manager", "goal 管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(); err != nil {
		return Goal{}, err
	}
	goal, err := m.load(strings.TrimSpace(id))
	if err != nil {
		return Goal{}, err
	}
	return cloneGoal(goal), nil
}

// LoadAll 返回所有可读的 goal，以及被跳过文件的诊断信息，
// 结果按 updated_at 降序、再按 id 升序排列。
func (m *Manager) LoadAll() ([]Goal, []SkippedFile) {
	if m == nil {
		return nil, []SkippedFile{{File: "-", Reason: "goal 管理器未初始化"}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(); err != nil {
		return nil, []SkippedFile{{File: "-", Reason: err.Error()}}
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, []SkippedFile{{File: "-", Reason: "读取 goals 目录失败：" + err.Error()}}
	}
	goals := make([]Goal, 0, len(entries))
	skipped := make([]SkippedFile, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, goalFileSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, goalFileSuffix)
		goal, loadErr := m.load(id)
		if loadErr != nil {
			skipped = append(skipped, SkippedFile{File: name, Reason: loadErr.Error()})
			continue
		}
		goals = append(goals, goal)
	}
	sort.Slice(goals, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339Nano, goals[i].UpdatedAt)
		tj, _ := time.Parse(time.RFC3339Nano, goals[j].UpdatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return goals[i].ID < goals[j].ID
	})
	return goals, skipped
}

// ValidateUpdateInput 在访问存储之前校验模型提供的更新参数。
// 对于未知状态或不会产生任何变化的更新，返回可诊断的错误。
func ValidateUpdateInput(status, note string, evidence []string, hasStatus, hasNote, hasEvidence bool) error {
	if !hasStatus && !hasNote && !hasEvidence {
		return newError("goal_invalid_argument", "至少提供 status、note 或 evidence 之一")
	}
	if hasStatus {
		status = strings.TrimSpace(status)
		if _, ok := validStatuses[status]; !ok {
			return newError("goal_invalid_status", "状态 %q 非法（应为 active、complete 或 blocked）", status)
		}
	}
	if hasEvidence {
		for _, item := range evidence {
			if strings.TrimSpace(item) == "" {
				return newError("goal_invalid_argument", "evidence 条目必须是非空字符串")
			}
		}
	}
	return nil
}

// validateTransition 强制约束 goal 生命周期。active 可以完成或阻塞；
// blocked 可以重新打开为 active；complete 是终态。
func validateTransition(from, to string) error {
	if from == to {
		if from == StatusComplete {
			return newError("goal_terminal", "goal 已完成；complete 是终态")
		}
		return nil
	}
	switch from {
	case StatusActive:
		if to == StatusComplete || to == StatusBlocked {
			return nil
		}
	case StatusBlocked:
		if to == StatusActive {
			return nil
		}
		return newError("goal_invalid_transition", "不能把 blocked goal 迁移到 %q；请先重新打开为 active", to)
	case StatusComplete:
		return newError("goal_terminal", "goal 已完成，不能再迁移到 %q", to)
	}
	return newError("goal_invalid_transition", "非法状态迁移 %q -> %q", from, to)
}

// Update 对单个 goal 应用部分更新：状态迁移、替换 note、追加 evidence。
// complete/blocked 更新要求结果 goal 上至少有一条 note 或 evidence 条目。
func (m *Manager) Update(id string, status string, note string, evidence []string, hasStatus, hasNote, hasEvidence bool) (Goal, error) {
	if m == nil {
		return Goal{}, newError("goal_no_manager", "goal 管理器未初始化")
	}
	if err := ValidateUpdateInput(status, note, evidence, hasStatus, hasNote, hasEvidence); err != nil {
		return Goal{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ready(); err != nil {
		return Goal{}, err
	}
	id = strings.TrimSpace(id)
	goal, err := m.load(id)
	if err != nil {
		return Goal{}, err
	}
	nextStatus := goal.Status
	if hasStatus {
		nextStatus = strings.TrimSpace(status)
		if err := validateTransition(goal.Status, nextStatus); err != nil {
			return Goal{}, err
		}
	} else if goal.Status == StatusComplete {
		return Goal{}, newError("goal_terminal", "goal 已完成，不能再修改")
	}
	nextNote := goal.Note
	if hasNote {
		nextNote = strings.TrimSpace(note)
	}
	nextEvidence := append([]string{}, goal.Evidence...)
	if hasEvidence {
		seen := make(map[string]struct{}, len(nextEvidence))
		for _, item := range nextEvidence {
			seen[item] = struct{}{}
		}
		for _, item := range evidence {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if _, dup := seen[item]; dup {
				continue
			}
			seen[item] = struct{}{}
			nextEvidence = append(nextEvidence, item)
		}
	}
	if nextStatus == StatusComplete || nextStatus == StatusBlocked {
		if nextNote == "" && len(nextEvidence) == 0 {
			return Goal{}, newError(
				"goal_missing_evidence",
				"状态 %q 需要至少一条说明结果或阻塞原因的 note 或 evidence", nextStatus)
		}
	}
	goal.Status = nextStatus
	goal.Note = nextNote
	goal.Evidence = nextEvidence
	goal.UpdatedAt = m.now().UTC().Format(time.RFC3339Nano)
	if err := m.write(goal); err != nil {
		return Goal{}, err
	}
	return cloneGoal(goal), nil
}

// Summaries 返回给定 goal 的精简投影（保持顺序）。
func Summaries(goals []Goal) []Summary {
	out := make([]Summary, 0, len(goals))
	for _, goal := range goals {
		out = append(out, Summary{
			ID:            goal.ID,
			Objective:     goal.Objective,
			Status:        goal.Status,
			UpdatedAt:     goal.UpdatedAt,
			EvidenceCount: len(goal.Evidence),
			Note:          goal.Note,
		})
	}
	return out
}

func cloneGoal(goal Goal) Goal {
	goal.Evidence = append([]string{}, goal.Evidence...)
	if goal.Evidence == nil {
		goal.Evidence = []string{}
	}
	return goal
}
