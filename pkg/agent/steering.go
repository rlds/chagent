package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rlds/chagent/pkg/bus"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/tools"
)

// SteeringMode 控制排队中的 steering 消息如何出队。
type SteeringMode string

const (
	// SteeringOneAtATime 每次轮询只取出第一条排队消息。
	SteeringOneAtATime SteeringMode = "one-at-a-time"
	// SteeringAll 在单次轮询中清空整个队列。
	SteeringAll SteeringMode = "all"
	// MaxQueueSize 是 Steering 队列中允许的消息数
	MaxQueueSize = 10
	// manualSteeringScope 是在没有活跃 turn/会话作用域时使用的
	// 旧版回退队列。
	manualSteeringScope = "__manual__"
)

// parseSteeringMode 将配置字符串规范化为 SteeringMode。
func parseSteeringMode(s string) SteeringMode {
	switch s {
	case "all":
		return SteeringAll
	default:
		return SteeringOneAtATime
	}
}

// steeringQueue 是一个线程安全的用户消息队列，这些消息可注入
// 正在运行的 agent 循环，在工具调用之间中断它。
type steeringQueue struct {
	mu     sync.Mutex
	queues map[string][]providers.Message
	mode   SteeringMode
}

func newSteeringQueue(mode SteeringMode) *steeringQueue {
	return &steeringQueue{
		queues: make(map[string][]providers.Message),
		mode:   mode,
	}
}

func normalizeSteeringScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return manualSteeringScope
	}
	return scope
}

// push 将 steering 消息入队到旧版回退作用域。
func (sq *steeringQueue) push(msg providers.Message) error {
	return sq.pushScope(manualSteeringScope, msg)
}

// pushScope 将 steering 消息入队到指定作用域。
func (sq *steeringQueue) pushScope(scope string, msg providers.Message) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	scope = normalizeSteeringScope(scope)
	queue := sq.queues[scope]
	if len(queue) >= MaxQueueSize {
		return fmt.Errorf("steering queue is full")
	}
	sq.queues[scope] = append(queue, msg)
	return nil
}

// dequeue 按配置的模式从旧版回退作用域移除并返回待处理的
// steering 消息。
func (sq *steeringQueue) dequeue() []providers.Message {
	return sq.dequeueScope(manualSteeringScope)
}

// dequeueScope 按配置的模式移除并返回指定作用域的待处理
// steering 消息。
func (sq *steeringQueue) dequeueScope(scope string) []providers.Message {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	return sq.dequeueLocked(normalizeSteeringScope(scope))
}

// dequeueScopeWithFallback 先清空作用域队列，为向后兼容
// 再回退到旧版手动作用域。
func (sq *steeringQueue) dequeueScopeWithFallback(scope string) []providers.Message {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	scope = strings.TrimSpace(scope)
	if scope != "" {
		if msgs := sq.dequeueLocked(scope); len(msgs) > 0 {
			return msgs
		}
	}

	return sq.dequeueLocked(manualSteeringScope)
}

func (sq *steeringQueue) dequeueLocked(scope string) []providers.Message {
	queue := sq.queues[scope]
	if len(queue) == 0 {
		return nil
	}

	switch sq.mode {
	case SteeringAll:
		msgs := append([]providers.Message(nil), queue...)
		delete(sq.queues, scope)
		return msgs
	default:
		msg := queue[0]
		queue[0] = providers.Message{} // 清除引用以便 GC 回收
		queue = queue[1:]
		if len(queue) == 0 {
			delete(sq.queues, scope)
		} else {
			sq.queues[scope] = queue
		}
		return []providers.Message{msg}
	}
}

// len 返回所有作用域中排队消息的总数。
func (sq *steeringQueue) len() int {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	total := 0
	for _, queue := range sq.queues {
		total += len(queue)
	}
	return total
}

// lenScope 返回指定作用域的排队消息数。
func (sq *steeringQueue) lenScope(scope string) int {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return len(sq.queues[normalizeSteeringScope(scope)])
}

func (sq *steeringQueue) clearScope(scope string) int {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	scope = normalizeSteeringScope(scope)
	count := len(sq.queues[scope])
	if count > 0 {
		delete(sq.queues, scope)
	}
	return count
}

// setMode 更新 steering 模式。
func (sq *steeringQueue) setMode(mode SteeringMode) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	sq.mode = mode
}

// getMode 返回当前 steering 模式。
func (sq *steeringQueue) getMode() SteeringMode {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sq.mode
}

// Steer 将一条用户消息入队，以便注入当前正在运行的 agent 循环。
// 该消息会在当前工具执行完成后被处理，从而导致该批次中剩余的
// 工具调用被跳过。
func (al *AgentLoop) Steer(msg providers.Message) error {
	scope := ""
	agentID := ""
	if ts := al.getAnyActiveTurnState(); ts != nil {
		scope = ts.sessionKey
		agentID = ts.agentID
	}
	return al.enqueueSteeringMessage(scope, agentID, msg)
}

func (al *AgentLoop) enqueueSteeringMessage(scope, agentID string, msg providers.Message) error {
	if al.steering == nil {
		return fmt.Errorf("steering queue is not initialized")
	}

	msg = steeringPromptMessage(msg)
	if err := al.steering.pushScope(scope, msg); err != nil {
		logger.WarnCF("agent", "Failed to enqueue steering message", map[string]any{
			"error": err.Error(),
			"role":  msg.Role,
			"scope": normalizeSteeringScope(scope),
		})
		return err
	}

	queueDepth := al.steering.lenScope(scope)
	logger.DebugCF("agent", "Steering message enqueued", map[string]any{
		"role":        msg.Role,
		"content_len": len(msg.Content),
		"media_count": len(msg.Media),
		"queue_len":   queueDepth,
		"scope":       normalizeSteeringScope(scope),
	})

	meta := HookMeta{
		Source:    "Steer",
		TracePath: "turn.interrupt.received",
	}
	if ts := al.getAnyActiveTurnState(); ts != nil {
		meta = ts.eventMeta("Steer", "turn.interrupt.received")
	} else {
		if strings.TrimSpace(agentID) != "" {
			meta.AgentID = agentID
		}
		normalizedScope := normalizeSteeringScope(scope)
		if normalizedScope != manualSteeringScope {
			meta.SessionKey = normalizedScope
		}
		if meta.AgentID == "" {
			if registry := al.GetRegistry(); registry != nil {
				if agent := registry.GetDefaultAgent(); agent != nil {
					meta.AgentID = agent.ID
				}
			}
		}
	}

	al.emitEvent(
		runtimeevents.KindAgentInterruptReceived,
		meta,
		InterruptReceivedPayload{
			Kind:       InterruptKindSteering,
			Role:       msg.Role,
			ContentLen: len(msg.Content),
			QueueDepth: queueDepth,
		},
	)

	return nil
}

// SteeringMode 返回当前 steering 模式。
func (al *AgentLoop) SteeringMode() SteeringMode {
	if al.steering == nil {
		return SteeringOneAtATime
	}
	return al.steering.getMode()
}

// SetSteeringMode 更新 steering 模式。
func (al *AgentLoop) SetSteeringMode(mode SteeringMode) {
	if al.steering == nil {
		return
	}
	al.steering.setMode(mode)
}

// dequeueSteeringMessages 是 agent 循环调用的内部方法，
// 用于在旧版回退作用域中轮询 steering 消息。
func (al *AgentLoop) dequeueSteeringMessages() []providers.Message {
	if al.steering == nil {
		return nil
	}
	return al.steering.dequeue()
}

func (al *AgentLoop) dequeueSteeringMessagesForScope(scope string) []providers.Message {
	if al.steering == nil {
		return nil
	}
	return al.steering.dequeueScope(scope)
}

func (al *AgentLoop) dequeueSteeringMessagesForScopeWithFallback(scope string) []providers.Message {
	if al.steering == nil {
		return nil
	}
	return al.steering.dequeueScopeWithFallback(scope)
}

func (al *AgentLoop) pendingSteeringCountForScope(scope string) int {
	if al.steering == nil {
		return 0
	}
	return al.steering.lenScope(scope)
}

func (al *AgentLoop) clearSteeringMessagesForScope(scope string) int {
	if al.steering == nil {
		return 0
	}
	return al.steering.clearScope(scope)
}

func (al *AgentLoop) continueWithSteeringMessages(
	ctx context.Context,
	agent *AgentInstance,
	sessionKey, channel, chatID string,
	scope *session.SessionScope,
	steeringMsgs []providers.Message,
) (string, error) {
	dispatch := DispatchRequest{
		SessionKey:   sessionKey,
		SessionScope: session.CloneScope(scope),
	}
	if channel != "" || chatID != "" {
		dispatch.InboundContext = &bus.InboundContext{
			Channel:  channel,
			ChatID:   chatID,
			ChatType: inferChatTypeFromSessionScope(scope),
		}
	}
	resp, _, _, err := al.runAgentLoop(ctx, agent, processOptions{
		Dispatch:                dispatch,
		DefaultResponse:         defaultResponse,
		EnableSummary:           true,
		SendResponse:            false,
		InitialSteeringMessages: steeringMsgs,
		SkipInitialSteeringPoll: true,
	})
	return resp, err
}

func (al *AgentLoop) agentForSession(sessionKey string) *AgentInstance {
	registry := al.GetRegistry()
	if registry == nil {
		return nil
	}

	agentIDs := registry.ListAgentIDs()
	sort.Strings(agentIDs)
	for _, agentID := range agentIDs {
		agent, ok := registry.GetAgent(agentID)
		if !ok || agent == nil {
			continue
		}
		resolvedAgentID := session.ResolveAgentID(agent.Sessions, sessionKey)
		if resolvedAgentID == "" {
			continue
		}
		if scopedAgent, ok := registry.GetAgent(resolvedAgentID); ok {
			return scopedAgent
		}
	}

	return registry.GetDefaultAgent()
}

// Continue 通过取出所有待处理的 steering 消息并让它们跑过 agent 循环，
// 来恢复一个空闲的 agent。它用于以下情况：agent 的最后一条消息来自
// assistant（即它已停止处理），而用户此后又入队了 steering 消息。
//
// 如果没有待处理的 steering 消息，则返回空字符串。
func (al *AgentLoop) Continue(ctx context.Context, sessionKey, channel, chatID string) (string, error) {
	// 用一个唯一的占位符占用会话，防止两个针对同一会话的并发 Continue 调用
	// 都通过活跃 turn 检查并创建并行 turn 的 TOCTOU 竞态。该占位符会在
	// continueWithSteeringMessages → runAgentLoop → registerActiveTurn 中
	// 被真实的 turnState 替换。
	placeholder := &turnState{
		turnID: "pending-continue-" + sessionKey + "-" + fmt.Sprintf("%d", al.turnSeq.Add(1)),
		phase:  TurnPhaseSetup,
	}
	if _, loaded := al.activeTurnStates.LoadOrStore(sessionKey, placeholder); loaded {
		if active := al.GetActiveTurnBySession(sessionKey); active != nil {
			return "", fmt.Errorf("turn %s is still active for session %q", active.TurnID, sessionKey)
		}
		// 另一个 Continue 刚刚占用了该槽位；让它处理这些 steering 消息。
		return "", nil
	}

	if err := al.ensureHooksInitialized(ctx); err != nil {
		al.activeTurnStates.Delete(sessionKey)
		return "", err
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		al.activeTurnStates.Delete(sessionKey)
		return "", err
	}

	steeringMsgs := al.dequeueSteeringMessagesForScopeWithFallback(sessionKey)
	if len(steeringMsgs) == 0 {
		al.activeTurnStates.Delete(sessionKey)
		return "", nil
	}

	agent := al.agentForSession(sessionKey)
	if agent == nil {
		al.activeTurnStates.Delete(sessionKey)
		return "", fmt.Errorf("no agent available for session %q", sessionKey)
	}

	if tool, ok := agent.Tools.Get("message"); ok {
		if resetter, ok := tool.(interface{ ResetSentInRound(sessionKey string) }); ok {
			resetter.ResetSentInRound(sessionKey)
		}
	}

	var scope *session.SessionScope
	if metaStore, ok := agent.Sessions.(session.MetadataAwareSessionStore); ok {
		scope = metaStore.GetSessionScope(sessionKey)
	}

	return al.continueWithSteeringMessages(ctx, agent, sessionKey, channel, chatID, scope, steeringMsgs)
}

func (al *AgentLoop) InterruptGraceful(hint string) error {
	ts := al.getAnyActiveTurnState()
	if ts == nil {
		return fmt.Errorf("no active turn")
	}
	if !ts.requestGracefulInterrupt(hint) {
		return fmt.Errorf("turn %s cannot accept graceful interrupt", ts.turnID)
	}

	al.emitEvent(
		runtimeevents.KindAgentInterruptReceived,
		ts.eventMeta("InterruptGraceful", "turn.interrupt.received"),
		InterruptReceivedPayload{
			Kind:    InterruptKindGraceful,
			HintLen: len(hint),
		},
	)

	return nil
}

// InterruptHard 终止任意一个活跃 turn。在并行模式下，它可能
// 作用于错误的会话。请优先使用 HardAbort(sessionKey)。
//
// Deprecated: 使用 HardAbort(sessionKey) 进行会话安全的终止。
func (al *AgentLoop) InterruptHard() error {
	ts := al.getAnyActiveTurnState()
	if ts == nil {
		return fmt.Errorf("no active turn")
	}
	if strings.HasPrefix(ts.turnID, "pending-") {
		return fmt.Errorf("turn is still initializing for session %s", ts.sessionKey)
	}
	if !ts.requestHardAbort() {
		return fmt.Errorf("turn %s is already aborting", ts.turnID)
	}

	al.emitEvent(
		runtimeevents.KindAgentInterruptReceived,
		ts.eventMeta("InterruptHard", "turn.interrupt.received"),
		InterruptReceivedPayload{
			Kind: InterruptKindHard,
		},
	)

	return nil
}

// ====================== SubTurn 结果轮询 ======================

// dequeuePendingSubTurnResults 轮询指定会话的 SubTurn 结果通道，
// 并在不阻塞的情况下返回所有可用结果。
// 如果该会话没有活跃的 turn 状态，则返回 nil。
func (al *AgentLoop) dequeuePendingSubTurnResults(sessionKey string) []*tools.ToolResult {
	tsInterface, ok := al.activeTurnStates.Load(sessionKey)
	if !ok {
		return nil
	}
	ts, ok := tsInterface.(*turnState)
	if !ok {
		return nil
	}

	var results []*tools.ToolResult
	for {
		select {
		case result, ok := <-ts.pendingResults:
			if !ok {
				return results
			}
			if result != nil {
				results = append(results, result)
			}
		default:
			return results
		}
	}
}

// ====================== 硬终止 ======================

// HardAbort 立即取消指定会话正在运行的 agent 循环，
// 并将取消级联到所有子 SubTurn。这是一个破坏性操作，
// 会在不等待优雅清理的情况下终止执行。
//
// 当用户明确要求立即终止（例如 "stop now"、"abort"）时使用它。
// 若需要允许 agent 完成当前工具并总结的优雅中断，
// 请改用 Steer()。
func (al *AgentLoop) HardAbort(sessionKey string) error {
	tsInterface, ok := al.activeTurnStates.Load(sessionKey)
	if !ok {
		return fmt.Errorf("no active turn state found for session %s", sessionKey)
	}

	ts, ok := tsInterface.(*turnState)
	if !ok {
		return fmt.Errorf("invalid turn state type for session %s", sessionKey)
	}

	if strings.HasPrefix(ts.turnID, "pending-") {
		return fmt.Errorf("turn is still initializing for session %s", sessionKey)
	}

	logger.InfoCF("agent", "Hard abort triggered", map[string]any{
		"session_key":            sessionKey,
		"turn_id":                ts.turnID,
		"depth":                  ts.depth,
		"initial_history_length": ts.initialHistoryLength,
	})

	// 立即取消活跃的 provider/工具 turn 上下文，使长时间运行的
	// 执行能尽快在根 turn 上停止。
	_ = ts.requestHardAbort()

	// IMPORTANT: 首先触发级联取消，以阻止所有子 SubTurn
	// 继续向会话添加消息。这可以防止在子任务仍在写入时
	// 执行回滚所导致的竞态条件。
	// 硬终止时使用 isHardAbort=true 立即取消所有子任务。
	ts.Finish(true)

	// 将会话历史回滚到 turn 开始之前的状态。
	if ts.session != nil {
		history := ts.session.GetHistory(sessionKey)
		if ts.initialHistoryLength < len(history) {
			ts.session.SetHistory(sessionKey, history[:ts.initialHistoryLength])
		}
	}

	return nil
}

// ====================== 后续消息注入 ======================

// InjectFollowUp 将一条消息入队，以便在当前 turn 完成后自动处理。
// 与会中断当前执行的 Steer() 不同，InjectFollowUp 会等待当前 turn
// 自然结束后再处理该消息。
//
// 它适用于：
// - 需要串联多个 turn 的自动化工作流
// - 应在主任务完成后运行的后台任务
// - 计划中的后续动作
//
// 当 agent 空闲时，该消息会通过 Continue() 处理。
func (al *AgentLoop) InjectFollowUp(msg providers.Message) error {
	// InjectFollowUp 使用与 Steer() 相同的 steering 队列机制，
	// 但语义差异在于调用时机：
	// - Steer() 在活跃执行期间调用，用于中断
	// - InjectFollowUp() 在规划后续工作时调用
	//
	// 两者最终进入同一个队列，并在 agent 空闲时
	// 由 Continue() 处理。
	return al.Steer(msg)
}

// ====================== 设计文档兼容的 API 别名 ======================

// InjectSteering 是 Steer() 的别名，用于匹配设计文档中的命名。
// 它将 steering 消息注入当前正在运行的 agent 循环。
func (al *AgentLoop) InjectSteering(msg providers.Message) error {
	return al.Steer(msg)
}
