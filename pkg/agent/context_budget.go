// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/tokenizer"
)

// isTurnBoundary 报告 msg 是否开启会话历史中的新回合。
// 回合是完整的“用户输入 → LLM 迭代 → 最终响应”循环（定义见 #1316）。
//
// 只有真实用户输入才开启回合。运行时注入的上下文消息
// （工作区指令、运行时快照、任务状态、技能目录、循环守卫等）
// 是其被注入回合内的搭车消息，绝不能被误认为回合起点——
// 否则压缩可能丢弃真实用户回合而只保留一段指令块，或把该块当成唯一的用户回合。
// 自主任务轮次上下文消息（protocoltypes.ContextKindTaskRound）是新自主轮次的
// 用户可见起点，因此它确实开启回合。
func isTurnBoundary(msg providers.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if !msg.IsContextMessage() {
		return true
	}
	return msg.Context.Kind == protocoltypes.ContextKindTaskRound
}

// parseTurnBoundaries 返回历史中每个回合的起始下标。
// 回合从回合边界（见 isTurnBoundary）开始，并延续所有后续 assistant/tool 消息，
// 直到下一个边界。
//
// 在回合边界切分可保证任何工具调用序列（assistant+ToolCalls → 工具结果）
// 都不会被切开。
func parseTurnBoundaries(history []providers.Message) []int {
	var starts []int
	for i, msg := range history {
		if isTurnBoundary(msg) {
			starts = append(starts, i)
		}
	}
	return starts
}

// isSafeBoundary 报告 index 是否为有效的 Turn 边界 —— 也就是说，
// 即保留部分（history[index:]）从真实用户回合开始的位置，
// 因此工具调用序列不会被撕开，注入上下文消息也不会被提升为会话的第一条保留用户消息。
func isSafeBoundary(history []providers.Message, index int) bool {
	if index <= 0 || index >= len(history) {
		return true
	}
	return isTurnBoundary(history[index])
}

// findSafeBoundary 查找距离 targetIndex 最近的 Turn 边界。
// 它优先选择位于 targetIndex 处或其之前的边界（以保留更近的上下文），
// 若没有则回退到 targetIndex 之后最近的边界；只有在完全不存在 Turn 边界时，
// 才原样返回 targetIndex。
func findSafeBoundary(history []providers.Message, targetIndex int) int {
	if len(history) == 0 {
		return 0
	}
	if targetIndex <= 0 {
		return 0
	}
	if targetIndex >= len(history) {
		return len(history)
	}

	turns := parseTurnBoundaries(history)
	if len(turns) == 0 {
		return targetIndex
	}

	// 查找位于 targetIndex 处或其之前的最后一个 Turn 边界。
	// 优先向前回退：这样可以保留更多较新的消息。
	backward := -1
	for _, t := range turns {
		if t <= targetIndex {
			backward = t
		}
	}
	if backward > 0 {
		return backward
	}

	// 目标之前没有有效的 Turn 边界（或只有 index 0 处的边界，
	// 而那会保留全部内容）。此时使用 targetIndex 之后的第一个 Turn。
	for _, t := range turns {
		if t > targetIndex {
			return t
		}
	}

	// targetIndex 之后同样没有 Turn 边界。唯一的边界位于 index 0，
	// 这意味着整个历史就是单个 Turn。返回 0 表示无法进行安全压缩 ——
	// 调用方会检查 mid <= 0，并在这种情况下跳过压缩。
	return 0
}

// EstimateMessageTokens 估算单条消息的 token 数。
// 委托给共享的 tokenizer 包，以保证 agent 与 seahorse 之间的一致性。
func EstimateMessageTokens(msg providers.Message) int {
	return tokenizer.EstimateMessageTokens(msg)
}

// EstimateToolDefsTokens 估算工具定义在 LLM 请求中占用的 token 总开销。
// 委托给共享的 tokenizer 包。
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
	return tokenizer.EstimateToolDefsTokens(defs)
}

// isOverContextBudget 检查组装后的消息加上工具定义与输出预留是否会
// 超出模型的上下文窗口。这样可以在调用 LLM 之前主动进行压缩，
// 而不是等到出现 400 错误后再被动处理。
func isOverContextBudget(
	contextWindow int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) bool {
	msgTokens := 0
	for _, m := range messages {
		msgTokens += EstimateMessageTokens(m)
	}

	toolTokens := EstimateToolDefsTokens(toolDefs)
	total := msgTokens + toolTokens + maxTokens

	return total > contextWindow
}

// trimHistoryToFitContextWindow 从逐步更新的历史切片重建提示词，
// 直到其能放入上下文窗口为止。最先丢弃最旧的完整回合，
// 以保证工具调用序列保持完整。
func trimHistoryToFitContextWindow(
	history []providers.Message,
	build func([]providers.Message) []providers.Message,
	contextWindow int,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) ([]providers.Message, []providers.Message, bool) {
	messages := build(history)
	if !isOverContextBudget(contextWindow, messages, toolDefs, maxTokens) {
		return history, messages, true
	}

	trimmedHistory := append([]providers.Message(nil), history...)
	for len(trimmedHistory) > 0 {
		dropUntil := nextHistoryTrimStart(trimmedHistory)
		if dropUntil <= 0 || dropUntil >= len(trimmedHistory) {
			trimmedHistory = nil
		} else {
			trimmedHistory = append([]providers.Message(nil), trimmedHistory[dropUntil:]...)
		}

		messages = build(trimmedHistory)
		if !isOverContextBudget(contextWindow, messages, toolDefs, maxTokens) {
			return trimmedHistory, messages, true
		}
	}

	return nil, messages, false
}

func nextHistoryTrimStart(history []providers.Message) int {
	if len(history) == 0 {
		return 0
	}

	turns := parseTurnBoundaries(history)
	if len(turns) >= 2 {
		return turns[1]
	}
	if len(turns) == 1 {
		if turns[0] > 0 {
			return turns[0]
		}
		return len(history)
	}

	return len(history)
}
