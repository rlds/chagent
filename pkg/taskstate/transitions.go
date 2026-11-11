package taskstate

import (
	"fmt"
	"strings"
	"time"
)

// CanTransition 报告阶段状态机是否允许从 from 转换到 to。
//
//	""       → active            （开始）
//	active   → paused/blocked/complete
//	paused   → active            （恢复）
//	blocked  → active            （恢复，需直接的人类请求）
//	complete → （终态）
func CanTransition(from, to Phase) bool {
	switch from {
	case "":
		return to == PhaseActive
	case PhaseActive:
		switch to {
		case PhasePaused, PhaseBlocked, PhaseComplete:
			return true
		}
	case PhasePaused, PhaseBlocked:
		return to == PhaseActive
	case PhaseComplete:
		return false
	}
	return false
}

// New 创建一个全新的 active 任务状态。
func New(objective string, maxRounds int, now time.Time) (State, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return State{}, fmt.Errorf("任务目标不能为空")
	}
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	if maxRounds < 1 || maxRounds > MaxRoundsLimit {
		return State{}, fmt.Errorf("max_rounds 必须在 1–%d 之间，当前为 %d", MaxRoundsLimit, maxRounds)
	}
	state := State{
		Version:        StateVersion,
		Objective:      objective,
		Phase:          PhaseActive,
		Round:          0,
		MaxRounds:      maxRounds,
		LastProgressAt: now,
		UpdatedAt:      now,
	}
	return state, nil
}

// Transition 执行不携带额外数据的普通阶段转换。
// 需要载荷或守卫的转换请使用 Pause/Resume/Complete/Block。
func (s *State) Transition(to Phase, now time.Time) error {
	if !CanTransition(s.Phase, to) {
		return fmt.Errorf("无效的阶段转换：%s → %s", phaseLabel(s.Phase), phaseLabel(to))
	}
	if to == PhaseBlocked {
		return fmt.Errorf("标记阻塞请使用 Block 并给出具体阻塞原因")
	}
	s.Phase = to
	s.UpdatedAt = now
	if to == PhaseActive {
		s.BlockedReason = nil
		s.BlockerFingerprint = ""
		s.BlockerRounds = 0
		s.BlockerLastRound = 0
	}
	return nil
}

// Pause 将 active 任务转为 paused。
func (s *State) Pause(now time.Time) error {
	if s.Phase != PhaseActive {
		return fmt.Errorf("只有 active 任务可以暂停，当前阶段为 %s", phaseLabel(s.Phase))
	}
	s.Phase = PhasePaused
	s.UpdatedAt = now
	return nil
}

// Resume 将 paused 或 blocked 任务恢复为 active。权限校验
// （blocked 任务需要直接的人类请求）由调用方负责；本方法只强制执行阶段状态机。
func (s *State) Resume(now time.Time) error {
	if s.Phase == PhaseComplete {
		return fmt.Errorf("已完成的任务不能恢复")
	}
	if !CanTransition(s.Phase, PhaseActive) {
		return fmt.Errorf("无法从阶段 %s 恢复", phaseLabel(s.Phase))
	}
	s.Phase = PhaseActive
	s.BlockedReason = nil
	s.BlockerFingerprint = ""
	s.BlockerRounds = 0
	s.BlockerLastRound = 0
	s.NoProgressRounds = 0
	s.LastProgressAt = now
	s.UpdatedAt = now
	return nil
}

// Complete 将 active 任务标记为完成。至少需要一条非空的证据：
// 模型必须引用具体产物，因此不能仅凭一句话声称完成。
func (s *State) Complete(evidence []string, now time.Time) error {
	if s.Phase != PhaseActive {
		return fmt.Errorf("只有 active 任务可以标记完成，当前阶段为 %s", phaseLabel(s.Phase))
	}
	cleaned := make([]string, 0, len(evidence))
	for _, item := range evidence {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		cleaned = append(cleaned, item)
	}
	if len(cleaned) == 0 {
		return fmt.Errorf("标记完成需要具体证据：请提供文件路径、测试输出、命令结果或用户确认")
	}
	s.Evidence = append(s.Evidence, cleaned...)
	s.MarkProgress(now)
	s.Phase = PhaseComplete
	s.UpdatedAt = now
	return nil
}

// CanBlock 报告已记录的阻塞条件是否已满足针对 reason 的
// 连续轮数阈值。
func (s *State) CanBlock(reason BlockedReason, threshold int) bool {
	if s.Phase != PhaseActive {
		return false
	}
	if threshold <= 0 {
		threshold = DefaultBlockedThreshold
	}
	if s.BlockerFingerprint == "" || s.BlockerFingerprint != reason.Fingerprint() {
		return false
	}
	return s.BlockerRounds >= threshold
}

// RecordBlockerAttempt 记录模型在当前轮次报告了 reason，
// 并报告是否满足连续轮数阈值。
//
// 计数按轮次进行：同一个 State.Round 内的重复调用不会推进计数器，
// 因此模型无法通过在一轮内调用三次工具来满足 3 轮规则。
func (s *State) RecordBlockerAttempt(
	reason BlockedReason,
	threshold int,
	now time.Time,
) (allowed bool, rounds int, err error) {
	if s.Phase != PhaseActive {
		return false, 0, fmt.Errorf("只有 active 任务可以标记阻塞，当前阶段为 %s", phaseLabel(s.Phase))
	}
	if err := reason.Validate(); err != nil {
		return false, 0, err
	}
	if threshold <= 0 {
		threshold = DefaultBlockedThreshold
	}
	fingerprint := reason.Fingerprint()
	switch {
	case s.BlockerFingerprint != fingerprint:
		s.BlockerFingerprint = fingerprint
		s.BlockerRounds = 1
		s.BlockerLastRound = s.Round
	case s.BlockerLastRound != s.Round:
		s.BlockerRounds++
		s.BlockerLastRound = s.Round
	}
	if s.BlockerRounds < 1 {
		s.BlockerRounds = 1
	}
	s.UpdatedAt = now
	return s.BlockerRounds >= threshold, s.BlockerRounds, nil
}

// Block 将 active 任务转为 blocked。同一个具体条件必须已经
// 持续至少 threshold 个连续轮次（默认 3）。
func (s *State) Block(reason BlockedReason, threshold int, now time.Time) error {
	if s.Phase != PhaseActive {
		return fmt.Errorf("只有 active 任务可以标记阻塞，当前阶段为 %s", phaseLabel(s.Phase))
	}
	if err := reason.Validate(); err != nil {
		return err
	}
	if threshold <= 0 {
		threshold = DefaultBlockedThreshold
	}
	if !s.CanBlock(reason, threshold) {
		return fmt.Errorf(
			"标记阻塞需要同一个具体条件连续至少 %d 轮；当前条件已记录 %d 轮",
			threshold, s.BlockerRounds,
		)
	}
	reasonCopy := reason
	s.Phase = PhaseBlocked
	s.BlockedReason = &reasonCopy
	s.UpdatedAt = now
	return nil
}

// ForceBlock 在不受模型上报阈值限制的情况下将任务标记为阻塞。
// 它保留给驱动器强制执行的守卫使用，例如轮次上限和无进展。
func (s *State) ForceBlock(code, message string, now time.Time) error {
	if s.Phase == PhaseComplete {
		return fmt.Errorf("已完成的任务不能再标记阻塞")
	}
	reason := BlockedReason{Code: strings.TrimSpace(code), Message: strings.TrimSpace(message)}
	if err := reason.Validate(); err != nil {
		return err
	}
	s.Phase = PhaseBlocked
	s.BlockedReason = &reason
	s.BlockerFingerprint = reason.Fingerprint()
	if s.BlockerRounds < DefaultBlockedThreshold {
		s.BlockerRounds = DefaultBlockedThreshold
	}
	s.UpdatedAt = now
	return nil
}

// MarkProgress 记录可验证的进展并重置无进展/阻塞计数器。
// 仅轮次增加不构成进展。
func (s *State) MarkProgress(now time.Time) {
	s.LastProgressAt = now
	s.NoProgressRounds = 0
	s.BlockerFingerprint = ""
	s.BlockerRounds = 0
	s.BlockerLastRound = 0
	s.UpdatedAt = now
}

// RecordNoProgressRound 递增连续无进展计数器
// 并返回新值。
func (s *State) RecordNoProgressRound(now time.Time) int {
	s.NoProgressRounds++
	s.UpdatedAt = now
	return s.NoProgressRounds
}

// RoundLimitReached 报告是否已达到轮次上限。
func (s *State) RoundLimitReached() bool {
	return s.MaxRounds > 0 && s.Round >= s.MaxRounds
}

// AdvanceRound 为下一个自主轮次递增轮次计数器。
func (s *State) AdvanceRound(now time.Time) error {
	if s.Phase != PhaseActive {
		return fmt.Errorf("只有 active 任务可以开始新一轮，当前阶段为 %s", phaseLabel(s.Phase))
	}
	if s.RoundLimitReached() {
		return fmt.Errorf("已达到最大轮次 %d", s.MaxRounds)
	}
	s.Round++
	s.UpdatedAt = now
	return nil
}

// ShouldContinue 报告自主驱动器是否可以安排下一轮。
func (s *State) ShouldContinue() bool {
	return s.Phase == PhaseActive && !s.RoundLimitReached()
}

// SetRoundLimitBlocked 记录驱动器强制执行的轮次上限停止。
func (s *State) SetRoundLimitBlocked(now time.Time) error {
	return s.ForceBlock("round-limit", fmt.Sprintf("已达到最大轮次 %d", s.MaxRounds), now)
}

// SetNoProgressBlocked 记录驱动器强制执行的无进展停止。
func (s *State) SetNoProgressBlocked(now time.Time) error {
	return s.ForceBlock(
		"no-progress",
		fmt.Sprintf("连续 %d 轮没有可验证的进展", s.NoProgressRounds),
		now,
	)
}

func phaseLabel(phase Phase) string {
	if phase == "" {
		return "(none)"
	}
	return string(phase)
}
