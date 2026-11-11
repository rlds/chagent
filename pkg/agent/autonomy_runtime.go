package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/taskstate"
	integrationtools "github.com/rlds/chagent/pkg/tools/integration"
)

const (
	// autonomyTurnSource 是自主续跑轮次使用的 processOptions.TurnSource 值。
	// 下游代码（事件、摘要发布、检查点）用它区分自主轮次
	// 与人类/调度器/subagent 回合。
	autonomyTurnSource = "autonomy"

	// autonomyRoundSourceDriver 标记由驱动在前一轮/回合结束后调度的轮次。
	autonomyRoundSourceDriver = "driver"

	// autonomyRoundSourceResume 标记由 resume-on-start 调度的轮次。
	autonomyRoundSourceResume = "resume"

	// autonomyEvidenceIterationPrefix 用于任务证据中的 max_tool_iterations 检查点。
	autonomyEvidenceIterationPrefix = "iteration-limit:"

	// autonomyEvidenceProviderPrefix 用于 provider 错误检查点。
	autonomyEvidenceProviderPrefix = "provider-error:"
)

// pendingAutonomyRound 是驱动侧为已调度（延迟）轮次保留的槽位。
// 每个会话最多一个槽位，避免某回合结束时另一个轮次仍在等待延迟而产生重复轮次。
type pendingAutonomyRound struct {
	cancel     context.CancelFunc
	agentID    string
	sessionKey string
	round      int
	maxRounds  int
	objective  string
	source     string
}

// renderTaskRoundPrompt 为单个自主续跑轮次构建中文 P-10 提示词。
func renderTaskRoundPrompt(state taskstate.State) string {
	return fmt.Sprintf("<task_round>\n目标：%q\n轮次：%d/%d\n\n"+
		"继续在同一会话中推进该目标。以当前工作区、工具结果和持久任务状态为准，不得以历史叙述代替当前核验。"+
		"执行具体动作并验证结果。声称完成前必须收集证据证明整个目标已达成，读取当前任务状态并标记完成。"+
		"仍有工作时保持任务为 active 进入下一轮；报告阻塞前遵循配置的完成/阻塞策略。\n</task_round>",
		state.Objective, state.Round, state.MaxRounds)
}

// autonomyReasonForState 把终态阶段映射为驱动停止原因。
func autonomyReasonForState(state taskstate.State) AutonomyRoundReason {
	switch state.Phase {
	case taskstate.PhaseComplete:
		return AutonomyRoundReasonCompleted
	case taskstate.PhaseBlocked:
		if state.BlockedReason != nil {
			switch state.BlockedReason.Code {
			case taskstate.BlockedCodeRoundLimit:
				return AutonomyRoundReasonRoundLimit
			case taskstate.BlockedCodeNoProgress:
				return AutonomyRoundReasonNoProgress
			}
		}
		return AutonomyRoundReasonBlocked
	case taskstate.PhasePaused:
		return AutonomyRoundReasonUserStop
	default:
		return ""
	}
}

// advanceAutonomyRoundGuarded 应用驱动侧的 P-18 守卫并推进轮次计数器。
// 它返回应被持久化的状态以及是否应调度新轮次。
// 只有在不调度新轮次时，返回的原因值才有意义。
func advanceAutonomyRoundGuarded(
	state taskstate.State,
	cfg config.AutonomyConfig,
	now time.Time,
) (taskstate.State, bool, AutonomyRoundReason) {
	if state.Phase != taskstate.PhaseActive {
		return state, false, autonomyReasonForState(state)
	}
	if cfg.MaxRounds > 0 && state.MaxRounds > cfg.MaxRounds {
		state.MaxRounds = cfg.MaxRounds
	}
	if state.RoundLimitReached() {
		_ = state.SetRoundLimitBlocked(now)
		return state, false, AutonomyRoundReasonRoundLimit
	}
	// 若某轮推进了计数器却未刷新 LastProgressAt，则没有可验证的进展。
	// 在决定是否调度前先计入该轮。
	if state.Round > 0 && state.LastProgressAt.Before(state.UpdatedAt) {
		state.RecordNoProgressRound(now)
	}
	threshold := cfg.NoProgressRounds
	if threshold <= 0 {
		threshold = taskstate.DefaultNoProgressRounds
	}
	if state.NoProgressRounds >= threshold {
		_ = state.SetNoProgressBlocked(now)
		return state, false, AutonomyRoundReasonNoProgress
	}
	if err := state.AdvanceRound(now); err != nil {
		return state, false, AutonomyRoundReasonEmptyRound
	}
	return state, true, ""
}

// advanceAutonomyRound 是现有上下文/运行时测试使用的兼容包装：它丢弃原因码。
func advanceAutonomyRound(state taskstate.State, cfg config.AutonomyConfig, now time.Time) (taskstate.State, bool) {
	next, schedule, _ := advanceAutonomyRoundGuarded(state, cfg, now)
	return next, schedule
}

// pendingStopRequested 报告该会话是否排队了用户停止请求。
// 与 takePendingStop 不同，它不消费该标记。
func (al *AgentLoop) pendingStopRequested(sessionKey string) bool {
	if al == nil {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false
	}
	_, ok := al.pendingStops.Load(sessionKey)
	return ok
}

func (al *AgentLoop) autonomyMapLocked() map[string]*pendingAutonomyRound {
	if al.autonomyPending == nil {
		al.autonomyPending = make(map[string]*pendingAutonomyRound)
	}
	return al.autonomyPending
}

// hasPendingAutonomyRound 报告该会话是否已有排队或延迟的自主轮次。只读。
func (al *AgentLoop) hasPendingAutonomyRound(sessionKey string) bool {
	if al == nil {
		return false
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return false
	}
	al.autonomyMu.Lock()
	defer al.autonomyMu.Unlock()
	_, ok := al.autonomyPending[key]
	return ok
}

// registerPendingAutonomyRound 占用每会话槽位。会话已有待处理轮次时失败，
// 这正是“每会话最多一个排队轮次”的实现方式。
func (al *AgentLoop) registerPendingAutonomyRound(sessionKey string, slot *pendingAutonomyRound) bool {
	if al == nil || slot == nil {
		return false
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return false
	}
	al.autonomyMu.Lock()
	defer al.autonomyMu.Unlock()
	pending := al.autonomyMapLocked()
	if _, exists := pending[key]; exists {
		return false
	}
	pending[key] = slot
	return true
}

// claimPendingAutonomyRound 在槽位仍是已注册的那个时原子地移除它。
// 槽位被取消或替换时返回 false。
func (al *AgentLoop) claimPendingAutonomyRound(sessionKey string, slot *pendingAutonomyRound) bool {
	if al == nil || slot == nil {
		return false
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return false
	}
	al.autonomyMu.Lock()
	defer al.autonomyMu.Unlock()
	if current, ok := al.autonomyPending[key]; !ok || current != slot {
		return false
	}
	delete(al.autonomyPending, key)
	return true
}

// cancelPendingAutonomyRound 取消某会话已排队/延迟的轮次。
// 新用户消息到达或用户停止任务时调用它，
// 使排队的自动工作绝不会覆盖明确的人类意图。
func (al *AgentLoop) cancelPendingAutonomyRound(sessionKey string, reason AutonomyRoundReason) bool {
	if al == nil {
		return false
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return false
	}
	al.autonomyMu.Lock()
	pending := al.autonomyMapLocked()
	slot := pending[key]
	if slot != nil {
		delete(pending, key)
	}
	al.autonomyMu.Unlock()
	if slot == nil {
		return false
	}
	if slot.cancel != nil {
		slot.cancel()
	}
	if reason == "" {
		reason = AutonomyRoundReasonUserStop
	}
	al.emitAutonomyRound(
		slot.agentID,
		slot.sessionKey,
		"",
		0,
		slot.round,
		slot.maxRounds,
		slot.objective,
		AutonomyRoundPhaseEnded,
		reason,
		slot.source,
	)
	return true
}

// cancelAllAutonomyRounds 取消所有排队轮次。用于停止/关闭。
func (al *AgentLoop) cancelAllAutonomyRounds(reason AutonomyRoundReason) int {
	if al == nil {
		return 0
	}
	al.autonomyMu.Lock()
	pending := al.autonomyMapLocked()
	slots := make([]*pendingAutonomyRound, 0, len(pending))
	for key, slot := range pending {
		slots = append(slots, slot)
		delete(pending, key)
	}
	al.autonomyMu.Unlock()
	if reason == "" {
		reason = AutonomyRoundReasonUserStop
	}
	for _, slot := range slots {
		if slot == nil {
			continue
		}
		if slot.cancel != nil {
			slot.cancel()
		}
		al.emitAutonomyRound(
			slot.agentID,
			slot.sessionKey,
			"",
			0,
			slot.round,
			slot.maxRounds,
			slot.objective,
			AutonomyRoundPhaseEnded,
			reason,
			slot.source,
		)
	}
	return len(slots)
}

func (al *AgentLoop) emitAutonomyRound(
	agentID, sessionKey, turnID string,
	iteration, round, maxRounds int,
	objective string,
	phase AutonomyRoundPhase,
	reason AutonomyRoundReason,
	source string,
) {
	if al == nil {
		return
	}
	al.emitEvent(
		runtimeevents.KindAgentAutonomyRound,
		HookMeta{
			AgentID:    agentID,
			TurnID:     turnID,
			SessionKey: sessionKey,
			Iteration:  iteration,
			Source:     autonomyTurnSource,
			TracePath:  "agent.autonomy.round",
		},
		AutonomyRoundPayload{
			Round:     round,
			MaxRounds: maxRounds,
			Phase:     phase,
			Reason:    reason,
			Objective: objective,
			Source:    source,
		},
	)
}

// emitAutonomyRoundState 是状态事件便捷包装。
func (al *AgentLoop) emitAutonomyRoundState(
	agentID, sessionKey, turnID string,
	iteration int,
	state taskstate.State,
	phase AutonomyRoundPhase,
	reason AutonomyRoundReason,
	source string,
) {
	al.emitAutonomyRound(
		agentID,
		sessionKey,
		turnID,
		iteration,
		state.Round,
		state.MaxRounds,
		state.Objective,
		phase,
		reason,
		source,
	)
}

// autonomyIntermediateEnabled 报告中间轮次内容是否允许发布。
// 默认 false（只有最终摘要可见）。
func autonomyIntermediateEnabled(cfg config.AutonomyConfig) bool {
	return cfg.SendIntermediate
}

// truncateText 为持久证据/用户可见提示缩短文本。
func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// evidenceHasPrefix 报告是否有证据项以 prefix 开头。
func evidenceHasPrefix(evidence []string, prefix string) bool {
	for _, item := range evidence {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

// renderAutonomyFinalSummary 构建自主任务到达 complete/blocked 时发送的
// 唯一用户可见最终摘要。它是纯中文文本，不是伪造的用户消息。
func renderAutonomyFinalSummary(state taskstate.State, reason AutonomyRoundReason) string {
	var b strings.Builder
	switch state.Phase {
	case taskstate.PhaseComplete:
		b.WriteString("任务已完成。\n")
	case taskstate.PhaseBlocked:
		b.WriteString("任务已阻塞，需要人工处理。\n")
	default:
		b.WriteString("自主任务已停止。\n")
	}
	fmt.Fprintf(&b, "目标：%s\n", state.Objective)
	fmt.Fprintf(&b, "阶段：%s\n", state.Phase)
	fmt.Fprintf(&b, "轮次：%d/%d\n", state.Round, state.MaxRounds)
	if reason != "" {
		fmt.Fprintf(&b, "停止原因：%s\n", reason)
	}
	if state.BlockedReason != nil {
		fmt.Fprintf(&b, "阻塞原因：%s（%s）\n", state.BlockedReason.Code, state.BlockedReason.Message)
	}
	if len(state.Evidence) > 0 {
		limit := 5
		if len(state.Evidence) < limit {
			limit = len(state.Evidence)
		}
		b.WriteString("证据：\n")
		for _, item := range state.Evidence[len(state.Evidence)-limit:] {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// publishAutonomyFinalSummary 每个终态只发布一次最终摘要。
// 外发消息是对任务原始 channel/chat 的普通 assistant 回复（SendResponse 语义）。
func (al *AgentLoop) publishAutonomyFinalSummary(
	ctx context.Context,
	ts *turnState,
	state taskstate.State,
	reason AutonomyRoundReason,
) {
	if al == nil || ts == nil {
		return
	}
	key := ts.sessionKey
	reasonCode := ""
	if state.BlockedReason != nil {
		reasonCode = state.BlockedReason.Code
	}
	fingerprint := fmt.Sprintf("%s|%s|%s|%d", state.Objective, state.Phase, reasonCode, state.Round)
	al.autonomyMu.Lock()
	if al.autonomySummarySent == nil {
		al.autonomySummarySent = make(map[string]string)
	}
	alreadySent := al.autonomySummarySent[key] == fingerprint
	if !alreadySent {
		al.autonomySummarySent[key] = fingerprint
	}
	al.autonomyMu.Unlock()
	if alreadySent {
		return
	}
	summary := renderAutonomyFinalSummary(state, reason)
	if summary == "" {
		return
	}
	al.PublishResponseIfNeeded(ctx, ts.channel, ts.chatID, key, summary, "", nil)
	logger.InfoCF("agent", "autonomy final summary published", map[string]any{
		"session_key": key,
		"phase":       string(state.Phase),
		"round":       state.Round,
	})
}

// publishAutonomyIntermediate 在操作者显式启用 SendIntermediate 时发布中间轮次内容。
// 默认配置下它是空操作，因此中间轮次保持静默。
func (al *AgentLoop) publishAutonomyIntermediate(ctx context.Context, ts *turnState, content string) {
	if al == nil || ts == nil || strings.TrimSpace(content) == "" {
		return
	}
	if ts.channel == "" && ts.chatID == "" {
		return
	}
	al.PublishResponseIfNeeded(ctx, ts.channel, ts.chatID, ts.sessionKey, content, "", nil)
}

// recordTaskProviderErrorCheckpoint 在任务活跃期间重试耗尽后持久化检查点。
// 它绝不会静默完成任务，并且每轮最多发送一条“需要恢复”提示。
func (al *AgentLoop) recordTaskProviderErrorCheckpoint(
	ctx context.Context,
	ts *turnState,
	err error,
) {
	if al == nil || ts == nil || err == nil || ts.agent == nil {
		return
	}
	// 只有 LLM provider 失败属于该检查点；hook 中止与会话持久化错误由各自的路径处理。
	// 用错误标识而不是错误文案判断，避免模型/用户可见文案本地化后分类失效。
	if !errors.Is(err, errLLMCallFailed) {
		return
	}
	store, ok := ts.agent.Sessions.(taskstate.Store)
	if !ok {
		return
	}
	state, ok := taskstate.Load(store, ts.sessionKey)
	if !ok || state.Phase != taskstate.PhaseActive {
		return
	}
	prefix := fmt.Sprintf("%sround=%d:", autonomyEvidenceProviderPrefix, state.Round)
	if evidenceHasPrefix(state.Evidence, prefix) {
		return
	}
	summary := truncateText(err.Error(), 160)
	state.Evidence = append(state.Evidence, prefix+summary)
	state.UpdatedAt = time.Now()
	if saveErr := store.SetTaskState(ts.sessionKey, state.Clone()); saveErr != nil {
		logger.WarnCF("agent", "autonomy: persist provider-error checkpoint failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       saveErr.Error(),
		})
		return
	}
	// 人类回合已通过正常发布路径暴露 provider 错误；
	// 只有自主轮次需要显式的恢复提示/事件。
	if ts.opts.TurnSource != autonomyTurnSource {
		return
	}
	al.emitAutonomyRound(
		ts.agent.ID,
		ts.sessionKey,
		ts.turnID,
		ts.currentIteration(),
		state.Round,
		state.MaxRounds,
		state.Objective,
		AutonomyRoundPhaseEnded,
		AutonomyRoundReasonProviderError,
		autonomyRoundSourceDriver,
	)
	al.PublishResponseIfNeeded(
		ctx,
		ts.channel,
		ts.chatID,
		ts.sessionKey,
		fmt.Sprintf("自主续跑在模型调用阶段失败，任务仍为 active，可从上一检查点恢复：%s", summary),
		"",
		nil,
	)
}

// recordIterationLimitCheckpoint 在任务仍活跃而回合达到 max_tool_iterations 时持久化检查点。
// 这里不推进轮次计数器；驱动在调度下一轮时推进。
func (ts *turnState) recordIterationLimitCheckpoint(al *AgentLoop) bool {
	if ts == nil || al == nil || ts.agent == nil {
		return false
	}
	state, ok := ts.activeTaskState()
	if !ok {
		return false
	}
	store, ok := ts.agent.Sessions.(taskstate.Store)
	if !ok {
		return false
	}
	prefix := fmt.Sprintf("%sround=%d:", autonomyEvidenceIterationPrefix, state.Round)
	if evidenceHasPrefix(state.Evidence, prefix) {
		return true
	}
	state.Evidence = append(
		state.Evidence,
		fmt.Sprintf("%sround=%d:max_tool_iterations=%d", autonomyEvidenceIterationPrefix, state.Round, ts.agent.MaxIterations),
	)
	state.UpdatedAt = time.Now()
	if err := store.SetTaskState(ts.sessionKey, state.Clone()); err != nil {
		logger.WarnCF("agent", "autonomy: persist iteration-limit checkpoint failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       err.Error(),
		})
		return false
	}
	return true
}

// maybeScheduleAutonomyRound 应用驱动守卫，并在会话仍有活动任务时调度下一个 P-10 轮次。
// 调度是异步的：一个轮次会开启新回合，因此 max_tool_iterations 和压缩边界不会被扭曲。
func (al *AgentLoop) maybeScheduleAutonomyRound(
	ctx context.Context,
	agent *AgentInstance,
	ts *turnState,
	result turnResult,
) {
	if al == nil || agent == nil || ts == nil {
		return
	}
	cfg := al.GetConfig()
	if cfg == nil {
		return
	}
	autonomyCfg := cfg.Agents.Defaults.Autonomy
	if !autonomyCfg.Enabled {
		return
	}
	// 子回合和无历史回合绝不驱动自主续跑。
	if ts.depth > 0 || ts.opts.NoHistory {
		return
	}
	// 只有正常结束的回合才可以继续任务。
	if result.status == TurnEndStatusAborted || result.status == TurnEndStatusError {
		return
	}
	store, ok := agent.Sessions.(taskstate.Store)
	if !ok {
		return
	}
	state, ok := taskstate.Load(store, ts.sessionKey)
	if !ok {
		return
	}
	ownRound := ts.opts.TurnSource == autonomyTurnSource
	// 用户停止/取消绝不能复活自动工作。
	if al.pendingStopRequested(ts.sessionKey) {
		al.cancelPendingAutonomyRound(ts.sessionKey, AutonomyRoundReasonUserStop)
		if ownRound {
			al.emitAutonomyRoundState(agent.ID, ts.sessionKey, ts.turnID, ts.currentIteration(), state,
				AutonomyRoundPhaseEnded, AutonomyRoundReasonUserStop, autonomyRoundSourceDriver)
		}
		return
	}
	if state.Phase != taskstate.PhaseActive {
		if ownRound {
			reason := autonomyReasonForState(state)
			al.emitAutonomyRoundState(agent.ID, ts.sessionKey, ts.turnID, ts.currentIteration(), state,
				AutonomyRoundPhaseEnded, reason, autonomyRoundSourceDriver)
			if !ts.opts.SendResponse {
				al.publishAutonomyFinalSummary(ctx, ts, state, reason)
			}
		}
		return
	}
	// 中间内容只有在显式启用时可见。
	if ownRound && autonomyIntermediateEnabled(autonomyCfg) && !ts.opts.SendResponse {
		al.publishAutonomyIntermediate(ctx, ts, result.finalContent)
	}
	// 排队的用户消息/steering 始终优先于自动续跑。
	if al.pendingSteeringCountForScope(ts.sessionKey) > 0 {
		al.cancelPendingAutonomyRound(ts.sessionKey, AutonomyRoundReasonUserStop)
		if ownRound {
			al.emitAutonomyRoundState(agent.ID, ts.sessionKey, ts.turnID, ts.currentIteration(), state,
				AutonomyRoundPhaseEnded, AutonomyRoundReasonUserStop, autonomyRoundSourceDriver)
		}
		return
	}
	// 每个会话最多一个排队/延迟轮次。
	if al.hasPendingAutonomyRound(ts.sessionKey) {
		return
	}
	// 另一个回合已占用该会话（例如并发的 Continue）。
	if al.getActiveTurnState(ts.sessionKey) != nil {
		return
	}

	next, schedule, reason := advanceAutonomyRoundGuarded(state, autonomyCfg, time.Now())
	if err := store.SetTaskState(ts.sessionKey, next.Clone()); err != nil {
		logger.WarnCF("agent", "autonomy: persist guarded state failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       err.Error(),
		})
		return
	}
	if !schedule {
		if ownRound {
			al.emitAutonomyRoundState(agent.ID, ts.sessionKey, ts.turnID, ts.currentIteration(), next,
				AutonomyRoundPhaseEnded, reason, autonomyRoundSourceDriver)
			// 驱动强制停止（轮次上限 / 无进展）是终态，
			// 并与模型报告的 blocked 使用同一用户可见摘要。
			if next.Phase == taskstate.PhaseBlocked && !ts.opts.SendResponse {
				al.publishAutonomyFinalSummary(ctx, ts, next, reason)
			}
		}
		return
	}
	if ownRound {
		// 回合正常结束，并已调度另一个轮次。
		al.emitAutonomyRoundState(agent.ID, ts.sessionKey, ts.turnID, ts.currentIteration(), state,
			AutonomyRoundPhaseEnded, "", autonomyRoundSourceDriver)
	}

	opts := ts.opts
	opts.UserMessage = renderTaskRoundPrompt(next)
	opts.Dispatch.UserMessage = opts.UserMessage
	opts.SendResponse = false
	opts.SuppressToolFeedback = true
	opts.NoHistory = false
	opts.SkipInitialSteeringPoll = true
	opts.InitialSteeringMessages = nil
	opts.TurnSource = autonomyTurnSource
	al.scheduleAutonomyRound(ctx, agent, opts, autonomyCfg, next, autonomyRoundSourceDriver)
}

// scheduleAutonomyRound 注册待处理轮次槽位并启动其延迟/定时 goroutine。
// 它绝不阻塞调用方。
func (al *AgentLoop) scheduleAutonomyRound(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
	autonomyCfg config.AutonomyConfig,
	state taskstate.State,
	source string,
) {
	if al == nil || agent == nil {
		return
	}
	sessionKey := strings.TrimSpace(opts.Dispatch.SessionKey)
	if sessionKey == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if source == "" {
		source = autonomyRoundSourceDriver
	}
	// 陈旧的已调度轮次不得在用户停止后存活。
	if al.pendingStopRequested(sessionKey) {
		return
	}
	if al.getActiveTurnState(sessionKey) != nil {
		return
	}
	// 延迟轮次会比调度它的回合活得更久，因此不得继承该回合的取消。
	// 单个取消由待处理轮次槽位（用户消息/停止）和 cancelAllAutonomyRounds 负责。
	baseCtx := context.WithoutCancel(ctx)
	roundCtx, cancel := context.WithCancel(baseCtx)
	slot := &pendingAutonomyRound{
		cancel:     cancel,
		agentID:    agent.ID,
		sessionKey: sessionKey,
		round:      state.Round,
		maxRounds:  state.MaxRounds,
		objective:  state.Objective,
		source:     source,
	}
	if !al.registerPendingAutonomyRound(sessionKey, slot) {
		cancel()
		return
	}
	al.emitAutonomyRoundState(agent.ID, sessionKey, "", 0, state,
		AutonomyRoundPhaseScheduled, "", source)

	delay := time.Duration(autonomyCfg.RoundDelayMS) * time.Millisecond
	if delay < 0 {
		delay = 0
	}
	go func() {
		defer cancel()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-roundCtx.Done():
				al.claimPendingAutonomyRound(sessionKey, slot)
				return
			case <-timer.C:
			}
		}
		if roundCtx.Err() != nil {
			al.claimPendingAutonomyRound(sessionKey, slot)
			return
		}
		// 槽位在轮次运行前释放：轮次运行期间由真实 turnState 拥有会话槽位。
		if !al.claimPendingAutonomyRound(sessionKey, slot) {
			return
		}
		al.runScheduledAutonomyRound(roundCtx, agent, sessionKey, opts, slot)
	}()
	logger.InfoCF("agent", "autonomy round scheduled", map[string]any{
		"session_key": sessionKey,
		"round":       state.Round,
		"max_rounds":  state.MaxRounds,
		"source":      source,
		"delay_ms":    autonomyCfg.RoundDelayMS,
	})
}

// runScheduledAutonomyRound 占用会话并运行一个自主轮次。
func (al *AgentLoop) runScheduledAutonomyRound(
	ctx context.Context,
	agent *AgentInstance,
	sessionKey string,
	opts processOptions,
	slot *pendingAutonomyRound,
) {
	if al == nil || agent == nil || slot == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	// 等待期间排队的用户消息/steering 优先：不要在其上再运行轮次。
	if al.pendingSteeringCountForScope(sessionKey) > 0 {
		al.emitAutonomyRound(
			agent.ID, sessionKey, "", 0,
			slot.round, slot.maxRounds, slot.objective,
			AutonomyRoundPhaseEnded, AutonomyRoundReasonUserStop, slot.source,
		)
		return
	}
	if al.pendingStopRequested(sessionKey) {
		return
	}
	// 在消耗 LLM 轮次前重新确认持久任务仍活跃。
	store, ok := agent.Sessions.(taskstate.Store)
	if !ok {
		return
	}
	state, ok := taskstate.Load(store, sessionKey)
	if !ok || state.Phase != taskstate.PhaseActive {
		return
	}
	// 保留会话槽位，使入站用户回合无法并行开始。
	// runTurn 会用真实 turnState 替换该占位符。
	placeholder := &turnState{
		turnID: makePendingTurnID(sessionKey, al.turnSeq.Add(1)),
		phase:  TurnPhaseSetup,
	}
	if _, loaded := al.activeTurnStates.LoadOrStore(sessionKey, placeholder); loaded {
		return
	}
	defer al.releaseSessionTurnState(sessionKey, placeholder)

	al.emitAutonomyRoundState(agent.ID, sessionKey, placeholder.turnID, 0, state,
		AutonomyRoundPhaseStarted, "", slot.source)

	if _, _, _, err := al.runAgentLoop(ctx, agent, opts); err != nil {
		if ctx.Err() != nil {
			return
		}
		logger.WarnCF("agent", "autonomy round failed", map[string]any{
			"session_key": sessionKey,
			"round":       slot.round,
			"error":       err.Error(),
		})
	}
	// 轮次期间到达的 steering 是真实用户输入：应在正常回合中处理它，
	// 而不是让它滞留。
	if ctx.Err() == nil && al.pendingSteeringCountForScope(sessionKey) > 0 {
		channel, chatID := opts.Dispatch.Channel(), opts.Dispatch.ChatID()
		resp, err := al.Continue(ctx, sessionKey, channel, chatID)
		if err != nil {
			logger.WarnCF("agent", "autonomy: continue steering failed", map[string]any{
				"session_key": sessionKey,
				"error":       err.Error(),
			})
		} else if resp != "" {
			al.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, resp, "", nil)
		}
	}
}

// resumeAutonomyTasksOnStart 在 autonomy.ResumeOnStart 启用时扫描已加载会话，
// 为每个活动任务调度一个轮次。它返回已调度的轮次数（便于日志/测试）。
func (al *AgentLoop) resumeAutonomyTasksOnStart(ctx context.Context) int {
	if al == nil {
		return 0
	}
	cfg := al.GetConfig()
	if cfg == nil || !cfg.Agents.Defaults.Autonomy.Enabled || !cfg.Agents.Defaults.Autonomy.ResumeOnStart {
		return 0
	}
	registry := al.GetRegistry()
	if registry == nil {
		return 0
	}
	scheduled := 0
	for _, agentID := range registry.ListAgentIDs() {
		agent, ok := registry.GetAgent(agentID)
		if !ok || agent == nil || agent.Sessions == nil {
			continue
		}
		store, ok := agent.Sessions.(taskstate.Store)
		if !ok {
			continue
		}
		for _, sessionKey := range agent.Sessions.ListSessions() {
			sessionKey = strings.TrimSpace(sessionKey)
			if sessionKey == "" {
				continue
			}
			state, ok := taskstate.Load(store, sessionKey)
			if !ok || state.Phase != taskstate.PhaseActive {
				continue
			}
			// 在调度前推进轮次计数器，使 resume 与轮内驱动遵循同一不变量：
			// State.Round 是将要运行的轮次。
			next, schedule, reason := advanceAutonomyRoundGuarded(state, cfg.Agents.Defaults.Autonomy, time.Now())
			if err := store.SetTaskState(sessionKey, next.Clone()); err != nil {
				logger.WarnCF("agent", "autonomy: persist resumed state failed", map[string]any{
					"session_key": sessionKey,
					"error":       err.Error(),
				})
				continue
			}
			if !schedule {
				al.emitAutonomyRoundState(agent.ID, sessionKey, "", 0, next,
					AutonomyRoundPhaseEnded, reason, autonomyRoundSourceResume)
				continue
			}
			state = next
			opts := processOptions{
				Dispatch: DispatchRequest{
					SessionKey:  sessionKey,
					UserMessage: renderTaskRoundPrompt(state),
				},
				DefaultResponse:         defaultResponse,
				EnableSummary:           true,
				SendResponse:            false,
				SuppressToolFeedback:    true,
				SkipInitialSteeringPoll: true,
				TurnSource:              autonomyTurnSource,
			}
			if channel, chatID := al.lastKnownTarget(); channel != "" || chatID != "" {
				opts.Dispatch.InboundContext = &bus.InboundContext{
					Channel:  channel,
					ChatID:   chatID,
					ChatType: "direct",
				}
			}
			al.scheduleAutonomyRound(ctx, agent, opts, cfg.Agents.Defaults.Autonomy, state, autonomyRoundSourceResume)
			scheduled++
		}
	}
	if scheduled > 0 {
		logger.InfoCF("agent", "autonomy: resumed active tasks on start", map[string]any{
			"count": scheduled,
		})
	}
	return scheduled
}

// lastKnownTarget 返回该工作区最近见过的 channel/chat。
// 持久状态把 "channel:chatID" 存为单个键；这里拆开它，
// 使恢复的轮次仍能投递最终摘要。
func (al *AgentLoop) lastKnownTarget() (channel, chatID string) {
	if al == nil || al.state == nil {
		return "", ""
	}
	raw := strings.TrimSpace(al.state.GetLastChannel())
	if raw == "" {
		return "", ""
	}
	if before, after, found := strings.Cut(raw, ":"); found {
		return before, after
	}
	return raw, ""
}

// pauseTaskStateForStop 在用户停止/取消回合时暂停活跃任务。
// 自动续跑绝不能复活用户已停止的工作。
func (al *AgentLoop) pauseTaskStateForStop(agent *AgentInstance, sessionKey string) {
	if agent == nil || agent.Sessions == nil {
		return
	}
	store, ok := agent.Sessions.(taskstate.Store)
	if !ok {
		return
	}
	state, ok := store.GetTaskState(sessionKey)
	if !ok || state.Phase != taskstate.PhaseActive {
		return
	}
	if err := state.Pause(time.Now()); err != nil {
		return
	}
	if err := store.SetTaskState(sessionKey, state); err != nil {
		logger.WarnCF("agent", "autonomy: pause task failed", map[string]any{
			"session_key": sessionKey,
			"error":       err.Error(),
		})
	}
}

// activeTaskState 在会话仍活跃时返回该会话的持久任务状态。
// 回合协调器用它来避免静默地终结空响应。
func (ts *turnState) activeTaskState() (taskstate.State, bool) {
	if ts == nil || ts.agent == nil || ts.agent.Sessions == nil {
		return taskstate.State{}, false
	}
	store, ok := ts.agent.Sessions.(taskstate.Store)
	if !ok {
		return taskstate.State{}, false
	}
	state, ok := store.GetTaskState(ts.sessionKey)
	if !ok || state.Phase != taskstate.PhaseActive {
		return taskstate.State{}, false
	}
	return state, true
}

// appendTaskContinuation 在模型未返回内容且任务仍活跃时，
// 向实时回合追加一条可见的任务轮次提醒。
func (ts *turnState) appendTaskContinuation(ctx context.Context, al *AgentLoop, exec *turnExecution, state taskstate.State) {
	if exec == nil {
		return
	}
	msg := providers.Message{
		Role:    "user",
		Content: renderTaskRoundPrompt(state),
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindTaskRound, Form: "prompt"},
	}
	exec.messages = append(exec.messages, msg)
	ts.persistRuntimeContextMessage(ctx, al, msg)
}

// turnSourceFromString 把 agent loop 的回合来源映射为任务工具的
// 权限枚举。空值默认视为 human，即入站用户回合。
func turnSourceFromString(value string) integrationtools.Source {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "scheduler", "heartbeat", "cron":
		return integrationtools.SourceScheduler
	case "autonomy":
		return integrationtools.SourceAutonomy
	case "subagent":
		return integrationtools.SourceSubagent
	case "system":
		return integrationtools.SourceSystem
	case "human", "":
		return integrationtools.SourceHuman
	default:
		return integrationtools.SourceUnknown
	}
}
