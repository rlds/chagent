// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/tools"
)

// =============================================================================
// TurnPhase - 表示回合当前所处的阶段
// =============================================================================

type TurnPhase string

const (
	TurnPhaseSetup      TurnPhase = "setup"
	TurnPhaseRunning    TurnPhase = "running"
	TurnPhaseTools      TurnPhase = "tools"
	TurnPhaseFinalizing TurnPhase = "finalizing"
	TurnPhaseCompleted  TurnPhase = "completed"
	TurnPhaseAborted    TurnPhase = "aborted"
)

// =============================================================================
// Control 信号 - 由 Pipeline 方法返回，用于驱动 runTurn 的协调器循环
// =============================================================================

type Control int

const (
	// ControlContinue 让协调器跳回回合循环顶部
	// （等价于原先的 "goto turnLoop"）。
	ControlContinue Control = iota
	// ControlBreak 让协调器退出回合循环并进入 Finalize。
	ControlBreak
	// ControlToolLoop 让协调器执行工具循环。
	ControlToolLoop
)

// ToolControl 信号由 ExecuteTools 返回，用于驱动工具循环的迭代。
type ToolControl int

const (
	// ToolControlContinue 让工具循环跳到下一次迭代
	// （例如有 pendingMessages 到达、有 SubTurn 结果等）。
	ToolControlContinue ToolControl = iota
	// ToolControlBreak 让工具循环退出并返回协调器。
	ToolControlBreak
	// ToolControlFinalize 告知协调器所有工具响应都已处理完毕，
	// 本回合应直接收尾，无需再调用一次 LLM。
	ToolControlFinalize
)

// LLMPhase 表示回合正在执行的阶段。
type LLMPhase int

const (
	LLMPhaseSetup LLMPhase = iota
	LLMPhasePreLLM
	LLMPhaseLLMCall
	LLMPhaseProcessing
	LLMPhaseToolLoop
	LLMPhaseTools
	LLMPhaseFinalizing
	LLMPhaseCompleted
	LLMPhaseAborted
)

// =============================================================================
// turnResult - runTurn 的返回值
// =============================================================================

type turnResult struct {
	finalContent string
	modelName    string
	usage        *providers.UsageInfo
	status       TurnEndStatus
	followUps    []bus.InboundMessage
}

// =============================================================================
// ActiveTurnInfo - 关于活动回合的公开信息
// =============================================================================

type ActiveTurnInfo struct {
	TurnID       string
	AgentID      string
	SessionKey   string
	Channel      string
	ChatID       string
	UserMessage  string
	Phase        TurnPhase
	Iteration    int
	StartedAt    time.Time
	Depth        int
	ParentTurnID string
	ChildTurnIDs []string
}

// =============================================================================
// turnExecution - 在回合循环各次迭代之间持续存在的可变状态
// =============================================================================

type turnExecution struct {
	// 核心消息状态（在整个回合中持续累积）
	messages         []providers.Message // 由 ContextBuilder 构建，每次迭代增长
	pendingMessages  []providers.Message // 等待注入的 steering/SubTurn 消息
	history          []providers.Message // 来自 ContextManager.Assemble
	summary          string
	currentTurnStart int

	// 回合输出
	finalContent string

	// 迭代计数
	iteration int

	// 由 Pipeline.PreLLM 设置的每次迭代状态
	activeCandidates  []providers.FallbackCandidate
	activeModel       string
	activeModelConfig *config.ModelConfig
	activeProvider    providers.LLMProvider
	usedLight         bool

	// LLM 调用的每次迭代状态
	response            *providers.LLMResponse
	normalizedToolCalls []providers.ToolCall
	allResponsesHandled bool
	streamingPublisher  *streamingChunkPublisher
	streamingFallback   bool
	suppressReasoning   bool
	callMessages        []providers.Message
	providerToolDefs    []providers.ToolDefinition
	llmModel            string
	llmModelName        string
	llmOpts             map[string]any
	gracefulTerminal    bool
	useNativeSearch     bool

	// 阶段跟踪
	phase LLMPhase

	// 供协调器使用的中止信号（由 Pipeline 方法设置）
	abortedByHardAbort bool // 在 LLM/工具执行期间触发硬中止时为 true
	abortedByHook      bool // 触发 HookActionAbortTurn 时为 true
}

// newTurnExecution 依据 turnState 与选项创建一个初始化好的 turnExecution。
func newTurnExecution(
	agent *AgentInstance,
	opts processOptions,
	history []providers.Message,
	summary string,
	messages []providers.Message,
) *turnExecution {
	return &turnExecution{
		history:          history,
		summary:          summary,
		messages:         messages,
		pendingMessages:  append([]providers.Message(nil), opts.InitialSteeringMessages...),
		currentTurnStart: len(messages),
		iteration:        0,
		phase:            LLMPhaseSetup,
	}
}

// =============================================================================
// turnState - 一个回合的完整状态，每个回合构造一次
// =============================================================================

type turnState struct {
	mu sync.RWMutex

	agent   *AgentInstance
	opts    processOptions
	profile config.EffectiveTurnProfile
	scope   turnEventScope

	turnID            string
	agentID           string
	sessionKey        string
	activeSkills      []string
	attemptedSkills   []string
	skillContextTrace []SkillContextSnapshot
	toolKinds         []string
	toolExecutions    []ToolExecutionRecord
	turnCtx           *TurnContext

	channel     string
	chatID      string
	workspace   string
	userMessage string
	media       []string

	phase        TurnPhase
	iteration    int
	startedAt    time.Time
	finalContent string

	followUps []bus.InboundMessage

	gracefulInterrupt     bool
	gracefulInterruptHint string
	gracefulTerminalUsed  bool
	hardAbort             bool
	providerCancel        context.CancelFunc
	turnCancel            context.CancelFunc

	restorePointHistory []providers.Message
	restorePointSummary string
	persistedMessages   []providers.Message

	// SubTurn 支持（沿用自 HEAD）
	depth          int                    // SubTurn 深度（根回合为 0）
	parentTurnID   string                 // 父回合 ID（根回合为空）
	childTurnIDs   []string               // 子回合 ID 列表
	pendingResults chan *tools.ToolResult // SubTurn 结果使用的通道
	// resultsMu 保护 pendingResults 的 close（Finish）与并发 send（deliverSubTurnResult）。
	// RWMutex：发送方取 RLock，唯一的关闭方取 Lock，因此 send 绝不会与 close 竞争。
	resultsMu            sync.RWMutex
	concurrencySem       chan struct{}        // 限制并发 SubTurn 数量的信号量
	isFinished           atomic.Bool          // 该回合是否已结束
	session              session.SessionStore // 会话存储引用
	initialHistoryLength int                  // 回合开始时历史长度的快照

	// 其他 SubTurn 字段
	ctx             context.Context    // 该回合的上下文
	cancelFunc      context.CancelFunc // 该回合上下文的取消函数
	critical        bool               // 父回合结束后该 SubTurn 是否应继续运行
	parentTurnState *turnState         // 指向父 turnState 的引用
	parentEnded     atomic.Bool        // 父回合是否已结束
	closeOnce       sync.Once          // 确保 pendingResults 通道只被关闭一次
	finishedChan    chan struct{}      // 回合结束时关闭

	// Token 预算跟踪
	tokenBudget      *atomic.Int64        // 共享的 token 预算计数器
	lastFinishReason string               // 最近一次 LLM finish_reason
	lastUsage        *providers.UsageInfo // 最近一次 LLM 用量信息

	// 反向引用所属的 AgentLoop（仅 SubTurn 设置，用于硬中止级联）
	al *AgentLoop

	// contextTouches 累积成功的文件工具路径，供下一次工作区上下文刷新使用
	// （T3.1）。
	contextTouches []string

	// pendingStepContexts 保存由工具执行排队（例如 loop-guard 通知）的合成上下文消息，
	// 由 pre-step 注入器在下一次 LLM 请求前刷新。它对应 DSH 的
	// “additionalContexts 被推入下一步收件箱”契约，因此工具绝不直接编辑 tool/assistant 消息。
	pendingStepContexts []providers.Message

	// loopGuard 检测本回合内连续相同的工具调用，并为下一次 pre-step 刷新排队提示。
	// 用户插话（steering）时重置它，因为跨新用户指令的重复不构成循环。
	loopGuard *repeatToolGuard

	// usageAnchor 是最近一次 provider 报告的 prompt token 数，与产生该请求的启发式估算配对
	// （用量锚定的上下文计量器）。
	usageAnchor *contextUsageAnchor

	// contextPressureNotice 是本回合已提示过的最高压力区间，
	// 使长回合不再重复接近上限通知。
	contextPressureNotice string
}

// =============================================================================
// turnState 构造函数与活动回合管理
// =============================================================================

func newTurnState(agent *AgentInstance, opts processOptions, scope turnEventScope) *turnState {
	ts := &turnState{
		agent:        agent,
		opts:         opts,
		profile:      opts.TurnProfile,
		scope:        scope,
		turnID:       scope.turnID,
		agentID:      agent.ID,
		sessionKey:   opts.Dispatch.SessionKey,
		activeSkills: activeSkillNames(agent, opts),
		turnCtx:      cloneTurnContext(scope.context),
		channel:      opts.Dispatch.Channel(),
		chatID:       opts.Dispatch.ChatID(),
		workspace:    agent.Workspace,
		userMessage:  opts.Dispatch.UserMessage,
		media:        append([]string(nil), opts.Dispatch.Media...),
		phase:        TurnPhaseSetup,
		startedAt:    time.Now(),
		loopGuard:    newRepeatToolGuard(DefaultLoopGuardConfig()),
	}

	// 绑定会话存储，并记录初始历史长度，供回滚逻辑使用
	if agent != nil && agent.Sessions != nil {
		ts.session = agent.Sessions
		history := agent.Sessions.GetHistory(opts.Dispatch.SessionKey)
		ts.initialHistoryLength = len(history)
		ts.restorePointHistory = append([]providers.Message(nil), history...)
		ts.restorePointSummary = agent.Sessions.GetSummary(opts.Dispatch.SessionKey)
	}

	return ts
}

func (al *AgentLoop) registerActiveTurn(ts *turnState) {
	al.activeTurnStates.Store(ts.sessionKey, ts)
}

func (al *AgentLoop) clearActiveTurn(ts *turnState) {
	al.releaseSessionTurnState(ts.sessionKey, ts)
}

func (al *AgentLoop) releaseSessionTurnState(sessionKey string, expected *turnState) {
	if expected == nil {
		al.activeTurnStates.Delete(sessionKey)
		return
	}
	if actual, ok := al.activeTurnStates.Load(sessionKey); ok && actual == expected {
		al.activeTurnStates.Delete(sessionKey)
	}
}

func (al *AgentLoop) getActiveTurnState(sessionKey string) *turnState {
	if val, ok := al.activeTurnStates.Load(sessionKey); ok {
		if ts, ok := val.(*turnState); ok {
			return ts
		}
		// 意外的非 *turnState 值——视为“无活动回合”，以免触发 panic。
		// 正常运行中不应出现这种情况。
	}
	return nil
}

// getAnyActiveTurnState 返回任意一个活动回合状态（用于向后兼容）
func (al *AgentLoop) getAnyActiveTurnState() *turnState {
	var firstTS *turnState
	al.activeTurnStates.Range(func(key, value any) bool {
		if ts, ok := value.(*turnState); ok {
			firstTS = ts
			return false
		}
		return true
	})
	return firstTS
}

func (al *AgentLoop) GetActiveTurn() *ActiveTurnInfo {
	// 为向后兼容，返回找到的第一个活动回合
	// 在新架构中，可能存在多个并发回合
	var firstTS *turnState
	al.activeTurnStates.Range(func(key, value any) bool {
		if ts, ok := value.(*turnState); ok {
			firstTS = ts
			return false
		}
		return true
	})
	if firstTS == nil {
		return nil
	}
	info := firstTS.snapshot()
	return &info
}

func (al *AgentLoop) GetActiveTurnBySession(sessionKey string) *ActiveTurnInfo {
	ts := al.getActiveTurnState(sessionKey)
	if ts == nil {
		return nil
	}
	info := ts.snapshot()
	return &info
}

// =============================================================================
// turnState - getter 与 setter
// =============================================================================

func (ts *turnState) snapshot() ActiveTurnInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	return ActiveTurnInfo{
		TurnID:       ts.turnID,
		AgentID:      ts.agentID,
		SessionKey:   ts.sessionKey,
		Channel:      ts.channel,
		ChatID:       ts.chatID,
		UserMessage:  ts.userMessage,
		Phase:        ts.phase,
		Iteration:    ts.iteration,
		StartedAt:    ts.startedAt,
		Depth:        ts.depth,
		ParentTurnID: ts.parentTurnID,
		ChildTurnIDs: append([]string(nil), ts.childTurnIDs...),
	}
}

func (ts *turnState) setPhase(phase TurnPhase) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.phase = phase
}

func (ts *turnState) setIteration(iteration int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.iteration = iteration
}

func (ts *turnState) currentIteration() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.iteration
}

func (ts *turnState) setFinalContent(content string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.finalContent = content
}

func (ts *turnState) finalContentLen() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.finalContent)
}

func (ts *turnState) finalContentSnapshot() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.finalContent
}

func (ts *turnState) recordToolKind(tool string) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, existing := range ts.toolKinds {
		if existing == tool {
			return
		}
	}
	ts.toolKinds = append(ts.toolKinds, tool)
}

func (ts *turnState) toolKindsSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]string(nil), ts.toolKinds...)
}

func (ts *turnState) recordToolExecution(tool string, success bool, errorSummary string, skillNames []string) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return
	}

	ts.recordToolKind(tool)

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolExecutions = append(ts.toolExecutions, ToolExecutionRecord{
		Name:         tool,
		Success:      success,
		ErrorSummary: strings.TrimSpace(errorSummary),
		SkillNames:   append([]string(nil), skillNames...),
	})
}

func (ts *turnState) toolExecutionsSnapshot() []ToolExecutionRecord {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.toolExecutions) == 0 {
		return nil
	}

	out := make([]ToolExecutionRecord, 0, len(ts.toolExecutions))
	for _, exec := range ts.toolExecutions {
		out = append(out, ToolExecutionRecord{
			Name:         exec.Name,
			Success:      exec.Success,
			ErrorSummary: exec.ErrorSummary,
			SkillNames:   append([]string(nil), exec.SkillNames...),
		})
	}
	return out
}

func (ts *turnState) recordAttemptedSkills(skillNames []string) {
	if len(skillNames) == 0 {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, skillName := range skillNames {
		skillName = strings.TrimSpace(skillName)
		if skillName == "" {
			continue
		}
		seen := false
		for _, existing := range ts.attemptedSkills {
			if existing == skillName {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		ts.attemptedSkills = append(ts.attemptedSkills, skillName)
	}
}

func (ts *turnState) attemptedSkillsSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]string(nil), ts.attemptedSkills...)
}

func (ts *turnState) recordSkillContextSnapshot(trigger string, skillNames []string) {
	if len(skillNames) == 0 {
		return
	}

	filtered := make([]string, 0, len(skillNames))
	for _, skillName := range skillNames {
		skillName = strings.TrimSpace(skillName)
		if skillName == "" {
			continue
		}
		filtered = append(filtered, skillName)
	}
	if len(filtered) == 0 {
		return
	}

	ts.recordAttemptedSkills(filtered)

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.skillContextTrace = append(ts.skillContextTrace, SkillContextSnapshot{
		Sequence:   len(ts.skillContextTrace) + 1,
		Trigger:    trigger,
		SkillNames: append([]string(nil), filtered...),
	})
}

func (ts *turnState) latestSkillContextSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.skillContextTrace) == 0 {
		return nil
	}
	return append([]string(nil), ts.skillContextTrace[len(ts.skillContextTrace)-1].SkillNames...)
}

func (ts *turnState) skillContextSnapshotsSnapshot() []SkillContextSnapshot {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.skillContextTrace) == 0 {
		return nil
	}

	snapshots := make([]SkillContextSnapshot, 0, len(ts.skillContextTrace))
	for _, snapshot := range ts.skillContextTrace {
		snapshots = append(snapshots, SkillContextSnapshot{
			Sequence:   snapshot.Sequence,
			Trigger:    snapshot.Trigger,
			SkillNames: append([]string(nil), snapshot.SkillNames...),
		})
	}
	return snapshots
}

func (ts *turnState) setTurnCancel(cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.turnCancel = cancel
}

func (ts *turnState) setProviderCancel(cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.providerCancel = cancel
}

func (ts *turnState) clearProviderCancel(_ context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.providerCancel = nil
}

func (ts *turnState) requestGracefulInterrupt(hint string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.hardAbort {
		return false
	}
	ts.gracefulInterrupt = true
	ts.gracefulInterruptHint = hint
	return true
}

func (ts *turnState) gracefulInterruptRequested() (bool, string) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.gracefulInterrupt && !ts.gracefulTerminalUsed, ts.gracefulInterruptHint
}

func (ts *turnState) markGracefulTerminalUsed() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.gracefulTerminalUsed = true
}

func (ts *turnState) requestHardAbort() bool {
	ts.mu.Lock()
	if ts.hardAbort {
		ts.mu.Unlock()
		return false
	}
	ts.hardAbort = true
	turnCancel := ts.turnCancel
	providerCancel := ts.providerCancel
	ts.mu.Unlock()

	if providerCancel != nil {
		providerCancel()
	}
	if turnCancel != nil {
		turnCancel()
	}
	return true
}

func (ts *turnState) hardAbortRequested() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.hardAbort
}

func (ts *turnState) eventMeta(source, tracePath string) HookMeta {
	snap := ts.snapshot()
	return HookMeta{
		AgentID:     snap.AgentID,
		TurnID:      snap.TurnID,
		SessionKey:  snap.SessionKey,
		Iteration:   snap.Iteration,
		Source:      source,
		TracePath:   tracePath,
		turnContext: cloneTurnContext(ts.turnCtx),
	}
}

func (ts *turnState) captureRestorePoint(history []providers.Message, summary string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.restorePointHistory = append([]providers.Message(nil), history...)
	ts.restorePointSummary = summary
}

func (ts *turnState) recordPersistedMessage(msg providers.Message) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.persistedMessages = append(ts.persistedMessages, msg)
}

func (ts *turnState) persistedMessagesSnapshot() []providers.Message {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]providers.Message(nil), ts.persistedMessages...)
}

func (ts *turnState) refreshRestorePointFromSession(agent *AgentInstance) {
	history := agent.Sessions.GetHistory(ts.sessionKey)
	summary := agent.Sessions.GetSummary(ts.sessionKey)

	persisted := ts.persistedMessagesSnapshot()

	if matched := matchingTurnMessageTail(history, persisted); matched > 0 {
		history = append([]providers.Message(nil), history[:len(history)-matched]...)
	}

	ts.captureRestorePoint(history, summary)
}

// ingestMessage 对一条已持久化的消息调用 ContextManager 的 Ingest 方法。
// 错误只记录日志，绝不阻塞回合。
func (ts *turnState) ingestMessage(ctx context.Context, al *AgentLoop, msg providers.Message) {
	if al.contextManager == nil {
		return
	}
	if err := al.contextManager.Ingest(ctx, &IngestRequest{
		SessionKey: ts.sessionKey,
		Message:    msg,
	}); err != nil {
		logger.WarnCF("agent", "Context manager ingest failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       err.Error(),
		})
	}
}

func (ts *turnState) restoreSession(agent *AgentInstance) error {
	ts.mu.RLock()
	history := append([]providers.Message(nil), ts.restorePointHistory...)
	summary := ts.restorePointSummary
	ts.mu.RUnlock()

	agent.Sessions.SetHistory(ts.sessionKey, history)
	agent.Sessions.SetSummary(ts.sessionKey, summary)
	return agent.Sessions.Save(ts.sessionKey)
}

func matchingTurnMessageTail(history, persisted []providers.Message) int {
	maxMatch := min(len(history), len(persisted))
	for size := maxMatch; size > 0; size-- {
		if messageSlicesEquivalent(history[len(history)-size:], persisted[len(persisted)-size:]) {
			return size
		}
	}
	return 0
}

func splitHistoryForActiveTurn(
	history []providers.Message,
	persisted []providers.Message,
) ([]providers.Message, []providers.Message) {
	matched := matchingTurnMessageTail(history, persisted)
	if matched <= 0 {
		return append([]providers.Message(nil), history...), nil
	}

	stable := append([]providers.Message(nil), history[:len(history)-matched]...)
	protected := append([]providers.Message(nil), history[len(history)-matched:]...)
	return stable, protected
}

func messageSlicesEquivalent(a, b []providers.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !messagesEquivalent(a[i], b[i]) {
			return false
		}
	}
	return true
}

func messagesEquivalent(a, b providers.Message) bool {
	return reflect.DeepEqual(normalizeMessageForComparison(a), normalizeMessageForComparison(b))
}

func normalizeMessageForComparison(msg providers.Message) providers.Message {
	msg.PromptLayer = ""
	msg.PromptSlot = ""
	msg.PromptSource = ""

	if len(msg.Media) == 0 {
		msg.Media = nil
	}
	if len(msg.Attachments) == 0 {
		msg.Attachments = nil
	}
	if len(msg.SystemParts) == 0 {
		msg.SystemParts = nil
	} else {
		msg.SystemParts = append([]providers.ContentBlock(nil), msg.SystemParts...)
		for i := range msg.SystemParts {
			msg.SystemParts[i].PromptLayer = ""
			msg.SystemParts[i].PromptSlot = ""
			msg.SystemParts[i].PromptSource = ""
		}
	}
	if len(msg.ToolCalls) == 0 {
		msg.ToolCalls = nil
	} else {
		msg.ToolCalls = append([]providers.ToolCall(nil), msg.ToolCalls...)
		for i := range msg.ToolCalls {
			msg.ToolCalls[i].Name = ""
			msg.ToolCalls[i].Arguments = nil
			msg.ToolCalls[i].ThoughtSignature = ""
			if msg.ToolCalls[i].Function != nil {
				fn := *msg.ToolCalls[i].Function
				fn.ThoughtSignature = ""
				msg.ToolCalls[i].Function = &fn
			}
		}
	}

	return msg
}

func (ts *turnState) interruptHintMessage() providers.Message {
	_, hint := ts.gracefulInterruptRequested()
	content := "已请求中断。停止安排工具调用，输出简短最终总结。"
	if hint != "" {
		content += "\n\n中断提示：" + hint
	}
	return interruptPromptMessage(content)
}

// =============================================================================
// SubTurn 相关方法
// =============================================================================

// Finish 把回合标记为已结束，并关闭 pendingResults 通道
func (ts *turnState) Finish(isHardAbort bool) {
	ts.isFinished.Store(true)

	// 只关闭 pendingResults 通道一次。写锁排除 deliverSubTurnResult 的任何在途 send
	// （它持有读锁）。
	ts.closeOnce.Do(func() {
		ts.resultsMu.Lock()
		if ts.pendingResults != nil {
			close(ts.pendingResults)
		}
		ts.resultsMu.Unlock()
		ts.mu.Lock()
		if ts.finishedChan == nil {
			ts.finishedChan = make(chan struct{})
		}
		close(ts.finishedChan)
		ts.mu.Unlock()
	})

	// 任何优雅收尾都必须通知直接子回合，使嵌套 SubTurn 能
	// 观察父回合的完成状态，并决定是停止还是继续。
	if !isHardAbort {
		ts.parentEnded.Store(true)
	}

	// 取消回合上下文
	if ts.cancelFunc != nil {
		ts.cancelFunc()
	}

	// 硬中止会级联到所有子回合
	if isHardAbort && ts.al != nil {
		ts.mu.RLock()
		children := append([]string(nil), ts.childTurnIDs...)
		ts.mu.RUnlock()
		for _, childID := range children {
			if val, ok := ts.al.activeTurnStates.Load(childID); ok {
				if child, ok := val.(*turnState); ok {
					child.Finish(true)
				}
			}
		}
	}
}

// Finished 返回回合是否已结束
func (ts *turnState) Finished() chan struct{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.finishedChan == nil {
		ts.finishedChan = make(chan struct{})
	}
	return ts.finishedChan
}

// IsParentEnded 检查父回合是否已结束
func (ts *turnState) IsParentEnded() bool {
	if ts.parentTurnState == nil {
		return false
	}
	return ts.parentTurnState.parentEnded.Load()
}

// GetLastFinishReason 返回最近一次 LLM 的 finish_reason
func (ts *turnState) GetLastFinishReason() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastFinishReason
}

// SetLastFinishReason 设置最近一次 LLM 的 finish_reason
func (ts *turnState) SetLastFinishReason(reason string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastFinishReason = reason
}

// GetLastUsage 返回最近一次 LLM 的用量信息
func (ts *turnState) GetLastUsage() *providers.UsageInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastUsage
}

// SetLastUsage 设置最近一次 LLM 的用量信息。
//
// provider 上报的用量属于不可信输入：负数计数（有缺陷或
// 恶意的 provider）会被钳制为 0，并且值会被拷贝，使 provider
// 之后的修改不会与读取方发生竞争。
func (ts *turnState) SetLastUsage(usage *providers.UsageInfo) {
	if usage == nil {
		ts.mu.Lock()
		ts.lastUsage = nil
		ts.mu.Unlock()
		return
	}
	sanitized := *usage
	if sanitized.PromptTokens < 0 {
		sanitized.PromptTokens = 0
	}
	if sanitized.CompletionTokens < 0 {
		sanitized.CompletionTokens = 0
	}
	if sanitized.TotalTokens < 0 {
		sanitized.TotalTokens = 0
	}
	if sanitized.CachedTokens < 0 {
		sanitized.CachedTokens = 0
	}
	if sanitized.CacheReadInputTokens < 0 {
		sanitized.CacheReadInputTokens = 0
	}
	if sanitized.CacheCreationInputTokens < 0 {
		sanitized.CacheCreationInputTokens = 0
	}
	if sanitized.ReasoningTokens < 0 {
		sanitized.ReasoningTokens = 0
	}
	ts.mu.Lock()
	ts.lastUsage = &sanitized
	ts.mu.Unlock()
}

// =============================================================================
// turnState 的上下文辅助函数
// =============================================================================

type turnStateKeyType struct{}

var turnStateKey = turnStateKeyType{}

func withTurnState(ctx context.Context, ts *turnState) context.Context {
	return context.WithValue(ctx, turnStateKey, ts)
}

func turnStateFromContext(ctx context.Context) *turnState {
	ts, _ := ctx.Value(turnStateKey).(*turnState)
	return ts
}

// TurnStateFromContext 从上下文中取出 turnState（为工具而导出）
func TurnStateFromContext(ctx context.Context) *turnState {
	return turnStateFromContext(ctx)
}
