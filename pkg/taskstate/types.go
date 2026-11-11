// Package taskstate 定义由自主续跑驱动器和任务控制工具使用的、
// 按会话持久化的任务状态。
//
// 本包刻意保持自包含：只导入 Go 标准库，因此可以被 pkg/memory、pkg/session、
// pkg/tools 和 pkg/agent 引用而不会引入循环依赖。
package taskstate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// StateVersion 是 New 写入的当前 schema 版本。
	StateVersion = 1

	// DefaultMaxRounds 是默认的自主轮次上限。
	DefaultMaxRounds = 20

	// MaxRoundsLimit 是 State.MaxRounds 允许的绝对上限。
	MaxRoundsLimit = 200

	// DefaultNoProgressRounds 是在驱动器判定任务阻塞之前，
	// 允许连续没有可验证进展的轮数。
	DefaultNoProgressRounds = 3

	// DefaultBlockedThreshold 是允许标记阻塞之前，
	// 同一个具体阻塞条件必须持续的最少连续轮数。
	DefaultBlockedThreshold = 3
)

// Phase 是会话任务的生命周期阶段。
type Phase string

const (
	PhaseActive   Phase = "active"
	PhasePaused   Phase = "paused"
	PhaseBlocked  Phase = "blocked"
	PhaseComplete Phase = "complete"
)

// Valid 报告 p 是否为已知阶段。
func (p Phase) Valid() bool {
	switch p {
	case PhaseActive, PhasePaused, PhaseBlocked, PhaseComplete:
		return true
	default:
		return false
	}
}

func (p Phase) String() string { return string(p) }

// 待办状态取值。Todo.Status 在序列化格式中保持为普通字符串，
// 以便 JSON schema 与提示词规范完全一致。
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// ValidTodoStatus 报告 status 是否为受支持的取值之一。
func ValidTodoStatus(status string) bool {
	switch status {
	case TodoPending, TodoInProgress, TodoCompleted:
		return true
	default:
		return false
	}
}

// Todo 是会话任务列表中的单个条目。
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Validate 检查 content 非空且 status 受支持。
func (t Todo) Validate() error {
	if strings.TrimSpace(t.Content) == "" {
		return fmt.Errorf("无效待办：`content` 必须是非空字符串")
	}
	if !ValidTodoStatus(t.Status) {
		return fmt.Errorf("无效待办：`status` 必须是 pending、in_progress 或 completed，当前为 %q", t.Status)
	}
	return nil
}

// 驱动器与任务工具使用的阻塞原因代码。
const (
	BlockedCodeRoundLimit    = "round-limit"
	BlockedCodeNoProgress    = "no-progress"
	BlockedCodeModelReported = "model-reported"
)

// IsValidCode 报告 code 是否为受支持的阻塞原因代码。
func IsValidCode(code string) bool {
	switch strings.TrimSpace(code) {
	case BlockedCodeRoundLimit, BlockedCodeNoProgress, BlockedCodeModelReported:
		return true
	default:
		return false
	}
}

// BlockedReason 描述任务进入阻塞阶段的原因。
type BlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Validate 检查 code 与 message 均非空。
func (r BlockedReason) Validate() error {
	if strings.TrimSpace(r.Code) == "" {
		return fmt.Errorf("无效阻塞原因：`code` 必须是非空字符串")
	}
	if strings.TrimSpace(r.Message) == "" {
		return fmt.Errorf("无效阻塞原因：`message` 必须是非空字符串")
	}
	return nil
}

// NormalizeBlockerMessage 压缩空白字符，使同一条件的
// 表述差异共享同一个指纹。
func NormalizeBlockerMessage(message string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(message)), " ")
}

// Fingerprint 返回由 code 与规范化后的 message 组成的稳定哈希。
// 驱动器用它检测“各轮次是否为同一具体条件”。
func (r BlockedReason) Fingerprint() string {
	sum := sha256.Sum256([]byte(
		strings.TrimSpace(r.Code) + "\n" + NormalizeBlockerMessage(r.Message),
	))
	return hex.EncodeToString(sum[:])
}

// State 是单个会话的持久化任务状态。
//
// BlockerLastRound 是阻塞阈值守卫使用的内部簿记：
// 它防止同一轮内的重复阻塞尝试被算作不同轮次。该字段会被持久化，
// 因此重启不会重置阈值。
type State struct {
	Version            int            `json:"version"`
	Objective          string         `json:"objective"`
	Phase              Phase          `json:"phase"`
	Todos              []Todo         `json:"todos,omitempty"`
	Round              int            `json:"round"`
	MaxRounds          int            `json:"max_rounds"`
	LastProgressAt     time.Time      `json:"last_progress_at,omitempty"`
	NoProgressRounds   int            `json:"no_progress_rounds,omitempty"`
	BlockerFingerprint string         `json:"blocker_fingerprint,omitempty"`
	BlockerRounds      int            `json:"blocker_rounds,omitempty"`
	BlockerLastRound   int            `json:"blocker_last_round,omitempty"`
	BlockedReason      *BlockedReason `json:"blocked_reason,omitempty"`
	Evidence           []string       `json:"evidence,omitempty"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// Clone 返回状态的深拷贝。
func (s State) Clone() State {
	cloned := s
	if s.Todos != nil {
		cloned.Todos = append([]Todo(nil), s.Todos...)
	}
	if s.Evidence != nil {
		cloned.Evidence = append([]string(nil), s.Evidence...)
	}
	if s.BlockedReason != nil {
		reason := *s.BlockedReason
		cloned.BlockedReason = &reason
	}
	return cloned
}

// IsZero 报告状态是否为空（尚未创建任务）。
func (s State) IsZero() bool {
	return s.Objective == "" && s.Phase == ""
}

// ValidationOptions 控制可选的校验规则。
type ValidationOptions struct {
	// AllowParallelInProgress 允许存在多个 in_progress 待办。
	AllowParallelInProgress bool
	// MaxRoundsLimit 覆盖默认的轮次上限。
	MaxRoundsLimit int
}

func (o ValidationOptions) maxRoundsLimit() int {
	if o.MaxRoundsLimit > 0 {
		return o.MaxRoundsLimit
	}
	return MaxRoundsLimit
}

// Validate 使用默认选项校验状态（存储层允许多个进行中的待办；
// todo_write 会施加更严格的产品规则）。
func (s State) Validate() error {
	return s.ValidateWith(ValidationOptions{AllowParallelInProgress: true})
}

// ValidateWith 校验状态的结构不变量。
func (s State) ValidateWith(opts ValidationOptions) error {
	if s.Version < 0 || s.Version > StateVersion {
		return fmt.Errorf("无效任务状态：version 必须为 %d，当前为 %d", StateVersion, s.Version)
	}
	if strings.TrimSpace(s.Objective) == "" {
		return fmt.Errorf("无效任务状态：objective 不能为空")
	}
	if !s.Phase.Valid() {
		return fmt.Errorf("无效任务状态：phase 无效：%q", s.Phase)
	}
	if s.MaxRounds < 1 || s.MaxRounds > opts.maxRoundsLimit() {
		return fmt.Errorf("无效任务状态：max_rounds 必须在 1–%d 之间，当前为 %d", opts.maxRoundsLimit(), s.MaxRounds)
	}
	if s.Round < 0 {
		return fmt.Errorf("无效任务状态：round 不能为负数")
	}
	if s.NoProgressRounds < 0 {
		return fmt.Errorf("无效任务状态：no_progress_rounds 不能为负数")
	}
	if s.BlockerRounds < 0 {
		return fmt.Errorf("无效任务状态：blocker_rounds 不能为负数")
	}
	if s.Phase == PhaseBlocked {
		if s.BlockedReason == nil {
			return fmt.Errorf("无效任务状态：blocked 阶段必须提供 blocked_reason")
		}
		if err := s.BlockedReason.Validate(); err != nil {
			return err
		}
	} else if s.BlockedReason != nil {
		return fmt.Errorf("无效任务状态：只有 blocked 阶段可以设置 blocked_reason")
	}
	if err := validateTodos(s.Todos, opts.AllowParallelInProgress); err != nil {
		return err
	}
	for i, evidence := range s.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return fmt.Errorf("无效任务状态：evidence[%d] 不能为空", i)
		}
	}
	return nil
}

func validateTodos(todos []Todo, allowParallelInProgress bool) error {
	seen := make(map[string]struct{}, len(todos))
	inProgress := 0
	for i, todo := range todos {
		if err := todo.Validate(); err != nil {
			return fmt.Errorf("无效任务状态：第 %d 项%s", i+1, strings.TrimPrefix(err.Error(), "无效待办："))
		}
		content := strings.TrimSpace(todo.Content)
		if _, exists := seen[content]; exists {
			return fmt.Errorf("无效任务状态：内容重复 %q", content)
		}
		seen[content] = struct{}{}
		if todo.Status == TodoInProgress {
			inProgress++
		}
	}
	if !allowParallelInProgress && inProgress > 1 {
		return fmt.Errorf("无效任务状态：最多只能有一个 in_progress（当前 %d 个）", inProgress)
	}
	return nil
}

// Store 是会话任务状态的最小持久化契约。它与 agent 循环
// 使用的可选 session.TaskStateStore 接口相匹配。
type Store interface {
	GetTaskState(sessionKey string) (State, bool)
	SetTaskState(sessionKey string, state State) error
}

// Load 从 store 读取任务状态。nil store 视为不支持，
// 返回 (State{}, false) 而不是 panic。
func Load(store Store, sessionKey string) (State, bool) {
	if store == nil || strings.TrimSpace(sessionKey) == "" {
		return State{}, false
	}
	state, ok := store.GetTaskState(sessionKey)
	if !ok {
		return State{}, false
	}
	if state.IsZero() {
		return State{}, false
	}
	return state.Clone(), true
}

// Save 校验并持久化 sessionKey 的状态。
func Save(store Store, sessionKey string, state State) error {
	if store == nil {
		return fmt.Errorf("当前会话不支持任务状态存储")
	}
	if strings.TrimSpace(sessionKey) == "" {
		return fmt.Errorf("任务状态需要所属的 session")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	if err := store.SetTaskState(sessionKey, state.Clone()); err != nil {
		return fmt.Errorf("写入任务状态失败：%w", err)
	}
	return nil
}
