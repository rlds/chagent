package agent

import (
	"github.com/rlds/chagent/pkg/bus"
)

// computeContextUsage 估算给定 agent 和会话当前的上下文窗口占用情况。
// 统计范围包括历史记录、系统提示词（含动态上下文、摘要和技能 —— 与
// BuildMessages 的组成方式保持一致）以及工具定义。
// 输出预留量（MaxTokens）不计入“已用”，但会缩减有效预算，这与
// isOverContextBudget 的压缩触发条件一致：
//
//	压缩条件：history + system + tools + maxTokens > contextWindow
//	等价于：history + system + tools > contextWindow - maxTokens
//
// 当 agent 或会话不可用时返回 nil。
func computeContextUsage(agent *AgentInstance, sessionKey string) *bus.ContextUsage {
	if agent == nil || agent.Sessions == nil {
		return nil
	}
	contextWindow := agent.ContextWindow
	if contextWindow <= 0 {
		return nil
	}

	// 历史记录 token 数
	history := agent.Sessions.GetHistory(sessionKey)
	historyTokens := 0
	for _, m := range history {
		historyTokens += EstimateMessageTokens(m)
	}

	// 系统消息 token 数：调用 EstimateSystemTokens，它与 BuildMessages 中
	// 完整的系统消息组成保持一致（静态提示词、动态上下文、激活的技能、
	// 带包装前缀的摘要）。
	systemTokens := 0
	if agent.ContextBuilder != nil {
		summary := agent.Sessions.GetSummary(sessionKey)
		// 激活技能传 nil：技能仅在用户通过 /use 显式激活时才会注入，这种情况
		// 很少见。传 nil 符合常见情形，并可避免把所有已安装技能都计算在内。
		systemTokens = agent.ContextBuilder.EstimateSystemTokens(summary, nil)
	}

	// 工具定义 token 数
	toolTokens := 0
	if agent.Tools != nil {
		toolTokens = EstimateToolDefsTokens(agent.Tools.ToProviderDefs())
	}

	// 已用 = 历史 + 系统（含摘要）+ 工具
	usedTokens := historyTokens + systemTokens + toolTokens

	// 有效预算 = contextWindow 减去输出预留量（maxTokens）
	effectiveWindow := contextWindow - agent.MaxTokens
	if effectiveWindow < 0 {
		effectiveWindow = contextWindow
	}

	// compressAt = effectiveWindow：与 isOverContextBudget 的主动触发条件
	// （msgTokens + toolTokens + maxTokens > contextWindow）保持一致。
	compressAt := effectiveWindow

	// summarizeAt = 软性摘要触发阈值：与 maybeSummarize 的阈值
	// （contextWindow * SummarizeTokenPercent / 100）一致。
	//
	// 引擎仅将它与历史消息 token 数比较（而非 UsedTokens）。HistoryTokens
	// 与 UsedTokens 一同暴露，以便 UI 同时展示两个值，避免用户困惑。
	summarizeAt := contextWindow * agent.SummarizeTokenPercent / 100
	if summarizeAt <= 0 {
		summarizeAt = compressAt
	}

	usedPercent := 0
	if compressAt > 0 {
		usedPercent = usedTokens * 100 / compressAt
	}
	if usedPercent > 100 {
		usedPercent = 100
	}

	return &bus.ContextUsage{
		UsedTokens:        usedTokens,
		TotalTokens:       contextWindow,
		HistoryTokens:     historyTokens,
		CompressAtTokens:  compressAt,
		SummarizeAtTokens: summarizeAt,
		UsedPercent:       usedPercent,
	}
}
