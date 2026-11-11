package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// legacyContextManager 把现有的摘要/压缩逻辑包装成
// ContextManager 实现。当没有配置其他
// ContextManager 时，它是默认实现。
type legacyContextManager struct {
	al          *AgentLoop
	summarizing sync.Map // 异步 Compact（轮次之后）的去重

	// writeMu 串行化强制压缩与异步摘要最终应用对会话的
	// 读-改-写序列。轮次进行中的上下文追加（AddFullMessage）保持无锁；
	// 摘要应用通过重新读取会话、只重写稳定前缀来兼顾这些追加（OPEN-04）。
	writeMu sync.Mutex
}

func (m *legacyContextManager) Assemble(_ context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	// 旧实现：从会话读取历史并原样返回。
	// 预算限制由 BuildMessages 调用方通过
	// isOverContextBudget + forceCompression 完成。
	agent := m.al.registry.GetDefaultAgent()
	if agent == nil {
		return &AssembleResponse{}, nil
	}
	history := agent.Sessions.GetHistory(req.SessionKey)
	summary := agent.Sessions.GetSummary(req.SessionKey)
	return &AssembleResponse{
		History: history,
		Summary: summary,
	}, nil
}

func (m *legacyContextManager) Compact(_ context.Context, req *CompactRequest) error {
	switch req.Reason {
	case ContextCompressReasonProactive, ContextCompressReasonRetry:
		// 同步紧急压缩——已超出预算。
		if result, ok := m.forceCompression(req.SessionKey); ok {
			m.al.emitEvent(
				runtimeevents.KindAgentContextCompress,
				m.al.newTurnEventScope("", req.SessionKey, nil).meta(0, "forceCompression", "turn.context.compress"),
				ContextCompressPayload{
					Reason:            req.Reason,
					DroppedMessages:   result.DroppedMessages,
					RemainingMessages: result.RemainingMessages,
				},
			)
		}
	case ContextCompressReasonSummarize:
		m.maybeSummarize(req.SessionKey)
	}
	return nil
}

func (m *legacyContextManager) Ingest(_ context.Context, _ *IngestRequest) error {
	// 旧实现：空操作。消息由 Sessions 的 JSONL 持久化。
	return nil
}

func (m *legacyContextManager) Clear(_ context.Context, sessionKey string) error {
	agent := m.al.registry.GetDefaultAgent()
	if agent == nil || agent.Sessions == nil {
		return fmt.Errorf("sessions not initialized")
	}
	agent.Sessions.SetHistory(sessionKey, []providers.Message{})
	agent.Sessions.SetSummary(sessionKey, "")
	return agent.Sessions.Save(sessionKey)
}

// maybeSummarize 在会话历史超过阈值时触发摘要。
// 它在 goroutine 中异步运行。
func (m *legacyContextManager) maybeSummarize(sessionKey string) {
	agent := m.al.registry.GetDefaultAgent()
	if agent == nil {
		return
	}

	newHistory := agent.Sessions.GetHistory(sessionKey)
	tokenEstimate := m.estimateTokens(newHistory)
	threshold := agent.ContextWindow * agent.SummarizeTokenPercent / 100

	if len(newHistory) > agent.SummarizeMessageThreshold || tokenEstimate > threshold {
		summarizeKey := agent.ID + ":" + sessionKey
		if _, loading := m.summarizing.LoadOrStore(summarizeKey, true); !loading {
			go func() {
				defer m.summarizing.Delete(summarizeKey)
				defer func() {
					if r := recover(); r != nil {
						logger.WarnCF("agent", "Summarization panic recovered", map[string]any{
							"session_key": sessionKey,
							"panic":       r,
						})
					}
				}()
				logger.Debug("Memory threshold reached. Optimizing conversation history...")
				m.summarizeSession(agent, sessionKey)
			}()
		}
	}
}

type compressionResult struct {
	DroppedMessages   int
	RemainingMessages int
}

// forceCompression 在触及上限时激进地缩减上下文。
// 它先运行确定性、无模型参与的工具结果裁剪（PruneToolResults）。
// 工具输出是最容易买回的上下文：保留每个回合、只丢弃超大工具结果的中间段，
// 可以保住对话结构。当裁剪本身就让估算请求回到 agent 上下文窗口内时，
// 只持久化裁剪后的历史，不丢弃任何回合。否则从裁剪后的历史中丢弃最旧的约 50% 回合
// （回合是完整的用户→LLM→响应循环，定义见 #1316），
// 因此工具调用序列绝不会被切开，注入上下文消息也绝不会被误认为用户回合。
func (m *legacyContextManager) forceCompression(sessionKey string) (compressionResult, bool) {
	agent := m.al.registry.GetDefaultAgent()
	if agent == nil {
		return compressionResult{}, false
	}

	// 与异步摘要的最终应用串行化：两者都会重写摘要和历史，
	// 因此它们的读-改-写序列不能交错。
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	history := agent.Sessions.GetHistory(sessionKey)
	if len(history) <= 2 {
		return compressionResult{}, false
	}

	existingSummary := agent.Sessions.GetSummary(sessionKey)

	// 把每个被裁剪工具结果的完整文本溢出保存，使模型能读回或 grep；
	// 替换标记携带定位符。nil 存储（无工作区）回退到无定位符标记，
	// 溢出失败时保持结果 inline，而不是宣传不存在的文件。
	spill := NewSpillStore(agent.Workspace)
	prunedHistory, prunedCount, savedChars, spilledCount := PruneToolResultsWithSpill(
		history, DefaultToolResultPruneConfig(), spill, sessionKey)

	if prunedCount > 0 && !m.isHistoryOverBudget(agent, prunedHistory, existingSummary) {
		// 仅裁剪就够了：保留每个回合并持久化更小的工具结果，
		// 让收益在重启后仍然存在。
		agent.Sessions.SetHistory(sessionKey, prunedHistory)
		agent.Sessions.Save(sessionKey)
		logger.WarnCF("agent", "Forced compression pruned tool results without dropping turns", map[string]any{
			"session_key":  sessionKey,
			"pruned_msgs":  prunedCount,
			"saved_chars":  savedChars,
			"spilled_msgs": spilledCount,
			"new_count":    len(prunedHistory),
		})
		return compressionResult{
			DroppedMessages:   0,
			RemainingMessages: len(prunedHistory),
		}, true
	}

	history = prunedHistory

	turns := parseTurnBoundaries(history)
	var mid int
	if len(turns) >= 2 {
		mid = turns[len(turns)/2]
	} else {
		mid = findSafeBoundary(history, len(history)/2)
	}
	var keptHistory []providers.Message
	if mid <= 0 {
		for i := len(history) - 1; i >= 0; i-- {
			if isTurnBoundary(history[i]) {
				keptHistory = []providers.Message{history[i]}
				break
			}
		}
	} else {
		keptHistory = history[mid:]
	}

	droppedCount := len(history) - len(keptHistory)

	compressionNote := fmt.Sprintf(
		"【紧急压缩】因超出上下文上限，已丢弃最早的 %d 条消息。",
		droppedCount,
	)
	if existingSummary != "" {
		compressionNote = existingSummary + "\n\n" + compressionNote
	}
	agent.Sessions.SetSummary(sessionKey, compressionNote)

	agent.Sessions.SetHistory(sessionKey, keptHistory)
	agent.Sessions.Save(sessionKey)

	logger.WarnCF("agent", "Forced compression executed", map[string]any{
		"session_key":  sessionKey,
		"dropped_msgs": droppedCount,
		"pruned_msgs":  prunedCount,
		"saved_chars":  savedChars,
		"spilled_msgs": spilledCount,
		"new_count":    len(keptHistory),
	})

	return compressionResult{
		DroppedMessages:   droppedCount,
		RemainingMessages: len(keptHistory),
	}, true
}

// isHistoryOverBudget 估算持久历史加摘要和输出预留是否会超出 agent 的上下文窗口。
// 该层拿不到工具定义，因此估算只覆盖消息侧；完整请求检查仍由 isOverContextBudget 负责。
func (m *legacyContextManager) isHistoryOverBudget(
	agent *AgentInstance,
	history []providers.Message,
	summary string,
) bool {
	messages := make([]providers.Message, 0, len(history)+1)
	if summary != "" {
		messages = append(messages, providers.Message{Role: "system", Content: summary})
	}
	messages = append(messages, history...)
	return isOverContextBudget(agent.ContextWindow, messages, nil, agent.MaxTokens)
}

func (m *legacyContextManager) summarizeSession(agent *AgentInstance, sessionKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	history := agent.Sessions.GetHistory(sessionKey)
	summary := agent.Sessions.GetSummary(sessionKey)

	if len(history) <= 4 {
		return
	}

	safeCut := findSafeBoundary(history, len(history)-4)
	if safeCut <= 0 {
		return
	}
	keepCount := len(history) - safeCut
	toSummarize := history[:safeCut]

	maxMessageTokens := agent.ContextWindow / 2
	validMessages := make([]providers.Message, 0)
	omitted := false

	for _, msg := range toSummarize {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if isSummarizableContextMessage(msg) {
			continue
		}
		msgTokens := len(msg.Content) / 2
		if msgTokens > maxMessageTokens {
			omitted = true
			continue
		}
		validMessages = append(validMessages, msg)
	}

	if len(validMessages) == 0 {
		return
	}

	const (
		maxSummarizationMessages = 10
		llmMaxRetries            = 3
	)

	var finalSummary string
	if len(validMessages) > maxSummarizationMessages {
		mid := len(validMessages) / 2
		mid = m.findNearestUserMessage(validMessages, mid)

		part1 := validMessages[:mid]
		part2 := validMessages[mid:]

		s1, _ := m.summarizeBatch(ctx, agent, part1, "")
		s2, _ := m.summarizeBatch(ctx, agent, part2, "")

		mergePrompt := fmt.Sprintf(
			"把以下两段对话摘要合并为一段连贯摘要：\n\n1：%s\n\n2：%s",
			s1, s2,
		)

		resp, err := m.retryLLMCall(ctx, agent, mergePrompt, llmMaxRetries)
		if err == nil && resp.Content != "" {
			finalSummary = resp.Content
		} else {
			finalSummary = s1 + " " + s2
		}
	} else {
		finalSummary, _ = m.summarizeBatch(ctx, agent, validMessages, summary)
	}

	if omitted && finalSummary != "" {
		finalSummary += "\n[说明：为提升效率，部分过长消息未纳入本摘要。]"
	}

	if finalSummary != "" {
		applied, kept := m.applySummaryIfUnchanged(
			agent, sessionKey, history, summary, finalSummary, keepCount)
		if applied {
			m.al.emitEvent(
				runtimeevents.KindAgentSessionSummarize,
				m.al.newTurnEventScope(agent.ID, sessionKey, nil).meta(0, "summarizeSession", "turn.session.summarize"),
				SessionSummarizePayload{
					SummarizedMessages: len(validMessages),
					KeptMessages:       kept,
					SummaryLen:         len(finalSummary),
					OmittedOversized:   omitted,
				},
			)
		}
	}
}

// applySummaryIfUnchanged 仅在会话仍与其（耗时数秒的）LLM 调用之前
// 所取的快照一致时，才应用摘要器的结果。
//
// OPEN-04：持久上下文注入（ContextSource / supersede / task-state）
// 会在摘要器等待模型期间追加消息。此时简单的
// TruncateHistory(keepCount) 会切进保留的尾部，静默丢弃
// 最近最旧的一个回合或刚刚注入的上下文消息。因此
// 我们改在 writeMu 下重新读取会话：
//   - 如果摘要或稳定前缀发生变化（压缩/摘要发生竞争），则
//     放弃过期的摘要（较新的写入者胜出；下一轮会重试）；
//   - 如果只是追加了新消息，则按追加增量偏移截断位置，
//     从而恰好丢弃已摘要的前缀，并完整保留
//     保留的尾部（包括调用期间追加的上下文）。
func (m *legacyContextManager) applySummaryIfUnchanged(
	agent *AgentInstance,
	sessionKey string,
	snapshotHistory []providers.Message,
	snapshotSummary string,
	finalSummary string,
	keepCount int,
) (bool, int) {
	if agent == nil || keepCount <= 0 {
		return false, 0
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	current := agent.Sessions.GetHistory(sessionKey)
	currentSummary := agent.Sessions.GetSummary(sessionKey)
	if currentSummary != snapshotSummary {
		logger.DebugCF("agent", "Summarize apply abandoned: summary changed during LLM call", map[string]any{
			"session_key":      sessionKey,
			"snapshot_summary": len(snapshotSummary),
			"current_summary":  len(currentSummary),
		})
		return false, 0
	}
	if len(current) < len(snapshotHistory) || !sameMessagePrefix(current, snapshotHistory) {
		logger.DebugCF("agent", "Summarize apply abandoned: history changed during LLM call", map[string]any{
			"session_key":    sessionKey,
			"snapshot_count": len(snapshotHistory),
			"current_count":  len(current),
		})
		return false, 0
	}

	appended := len(current) - len(snapshotHistory)
	kept := keepCount + appended
	if kept <= 0 || kept > len(current) {
		return false, 0
	}

	agent.Sessions.SetSummary(sessionKey, finalSummary)
	agent.Sessions.TruncateHistory(sessionKey, kept)
	agent.Sessions.Save(sessionKey)
	return true, kept
}

// sameMessagePrefix 报告 current[:len(prefix)] 是否与
// prefix 完全相同。两个切片都来自同一个会话存储；JSONL 重放会产生
// 相等的值，因此深度相等是最廉价且可靠的版本检查。
func sameMessagePrefix(current, prefix []providers.Message) bool {
	if len(current) < len(prefix) {
		return false
	}
	return reflect.DeepEqual(current[:len(prefix)], prefix)
}

// isSummarizableContextMessage 报告 msg 是否为运行时注入、必须排除在对话摘要之外的上下文。
// 上下文消息携带大型、瞬态的指令/任务/状态块，上下文投影器本来就会重建它们，
// 把它们写进摘要只会让摘要充斥非对话材料。
// 自主任务轮次消息是真实的用户可见轮次起点，因此保留在摘要中。
func isSummarizableContextMessage(msg providers.Message) bool {
	return msg.IsContextMessage() && msg.Context.Kind != protocoltypes.ContextKindTaskRound
}

func (m *legacyContextManager) findNearestUserMessage(messages []providers.Message, mid int) int {
	originalMid := mid

	for mid > 0 && !isTurnBoundary(messages[mid]) {
		mid--
	}

	if isTurnBoundary(messages[mid]) {
		return mid
	}

	mid = originalMid
	for mid < len(messages) && !isTurnBoundary(messages[mid]) {
		mid++
	}

	if mid < len(messages) {
		return mid
	}

	return originalMid
}

func (m *legacyContextManager) retryLLMCall(
	ctx context.Context,
	agent *AgentInstance,
	prompt string,
	maxRetries int,
) (*providers.LLMResponse, error) {
	const llmTemperature = 0.3

	var resp *providers.LLMResponse
	var err error

	for attempt := 0; attempt < maxRetries; attempt++ {
		m.al.activeRequestsInc()
		resp, err = func() (*providers.LLMResponse, error) {
			defer m.al.activeRequestsDec()
			return agent.Provider.Chat(
				ctx,
				[]providers.Message{{Role: "user", Content: prompt}},
				nil,
				agent.Model,
				map[string]any{
					"max_tokens":       agent.MaxTokens,
					"temperature":      llmTemperature,
					"prompt_cache_key": agent.ID,
				},
			)
		}()

		if err == nil && resp != nil && resp.Content != "" {
			return resp, nil
		}
		if attempt < maxRetries-1 {
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}

	return resp, err
}

func (m *legacyContextManager) summarizeBatch(
	ctx context.Context,
	agent *AgentInstance,
	batch []providers.Message,
	existingSummary string,
) (string, error) {
	const (
		llmMaxRetries             = 3
		fallbackMinContentLength  = 200
		fallbackMaxContentPercent = 10
	)

	var sb strings.Builder
	sb.WriteString("为以下对话片段生成简明摘要，保留核心上下文与关键要点。\n")
	if existingSummary != "" {
		sb.WriteString("已有上下文：")
		sb.WriteString(existingSummary)
		sb.WriteString("\n")
	}
	sb.WriteString("\n对话内容：\n")
	for _, msg := range batch {
		fmt.Fprintf(&sb, "%s: %s\n", msg.Role, msg.Content)
	}
	prompt := sb.String()

	response, err := m.retryLLMCall(ctx, agent, prompt, llmMaxRetries)
	if err == nil && response.Content != "" {
		return strings.TrimSpace(response.Content), nil
	}

	var fallback strings.Builder
	fallback.WriteString("对话摘要：")
	for i, msg := range batch {
		if i > 0 {
			fallback.WriteString(" | ")
		}
		content := strings.TrimSpace(msg.Content)
		runes := []rune(content)
		if len(runes) == 0 {
			fallback.WriteString(fmt.Sprintf("%s: ", msg.Role))
			continue
		}

		keepLength := len(runes) * fallbackMaxContentPercent / 100
		if keepLength < fallbackMinContentLength {
			keepLength = fallbackMinContentLength
		}
		if keepLength > len(runes) {
			keepLength = len(runes)
		}

		content = string(runes[:keepLength])
		if keepLength < len(runes) {
			content += "..."
		}
		fallback.WriteString(fmt.Sprintf("%s: %s", msg.Role, content))
	}
	return fallback.String(), nil
}

func (m *legacyContextManager) estimateTokens(messages []providers.Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateMessageTokens(msg)
	}
	return total
}
