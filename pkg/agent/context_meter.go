// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// 上下文计量器：以用量为锚点的上下文压力估算。
//
// DSH 的 token-meter 是一个投影，从单一事实来源回答“上下文现在有多满？”。
// chagent 用启发式分词器（pkg/tokenizer）估算，且没有按会话的投影，因此估算值
// 可能与 provider 实际计费值漂移——尤其是 CJK 文本、工具 schema 和缓存前缀。
// 本文件补上 DSH 机制中最小的有用部分：每次成功响应后，用 provider 报告的
// prompt_tokens 作为锚点，后续估算按观测到的比例校正。结果只用于生成提示性的
// 接近上限通知；压缩仍以 isOverContextBudget 为硬性守卫，所以锚点偏差只会让
// 通知偏早或偏晚，绝不会破坏历史。
//
// 迁移方向：当存在真正的 SessionStore 投影后，把锚点移过去（按会话键），并让压缩
// 读取 measure() 而不是本地比例。

// contextPressureNoticeRatios 是（相对有效 prompt 预算的）压力比例阈值，
// 达到即注入接近上限的通知。每个区间每轮最多提示一次，避免长轮次用重复告警
// 塞满上下文。
var contextPressureNoticeRatios = []float64{0.85, 0.95}

// contextUsageAnchor 保存最近一次 provider 报告的 prompt 大小，以及产生该请求的
// 启发式估算值。
type contextUsageAnchor struct {
	promptTokens    int
	estimatedTokens int
	at              time.Time
}

// correctionRatio 返回把启发式估算映射到 provider 报告 token 数的乘数。
// 锚点为零或负数表示“不校正”（比例 1）。
func (a *contextUsageAnchor) correctionRatio() float64 {
	if a == nil || a.promptTokens <= 0 || a.estimatedTokens <= 0 {
		return 1
	}
	ratio := float64(a.promptTokens) / float64(a.estimatedTokens)
	// 防御异常锚点（例如只报告缓存前缀计数的 provider）：钳制到合理区间，
	// 让通知不会剧烈摆动。
	if ratio < 0.25 {
		return 0.25
	}
	if ratio > 4 {
		return 4
	}
	return ratio
}

// estimateRequestTokens 返回单个请求的启发式 token 估算。
func estimateRequestTokens(messages []providers.Message, toolDefs []providers.ToolDefinition) int {
	total := 0
	for _, msg := range messages {
		total += EstimateMessageTokens(msg)
	}
	total += EstimateToolDefsTokens(toolDefs)
	return total
}

// recordUsageAnchor 保存 provider 报告的 prompt 大小及产生该请求的估算值。
// 每次成功的 LLM 响应后调用；usage 为空则忽略。
func (ts *turnState) recordUsageAnchor(
	usage *providers.UsageInfo,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
) {
	if ts == nil || usage == nil || usage.PromptTokens <= 0 {
		return
	}
	estimated := estimateRequestTokens(messages, toolDefs)
	if estimated <= 0 {
		return
	}
	ts.mu.Lock()
	ts.usageAnchor = &contextUsageAnchor{
		promptTokens:    usage.PromptTokens,
		estimatedTokens: estimated,
		at:              time.Now(),
	}
	ts.mu.Unlock()
}

// usageAnchorSnapshot 返回当前锚点的副本（无锚点时为 nil）。
func (ts *turnState) usageAnchorSnapshot() *contextUsageAnchor {
	if ts == nil {
		return nil
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.usageAnchor == nil {
		return nil
	}
	copied := *ts.usageAnchor
	return &copied
}

// contextPromptBudget 返回接近上限通知使用的 prompt token 预算。
// 这里把 DSH 的公式适配到 chagent 较小的默认窗口：
//
//	budget = min(contextWindow * thresholdRatio, contextWindow - maxOutput)
//
// DSH 还会再减去 headroomTokens（默认 65536），这比 chagent 默认的 32768 窗口
// 还大；无条件相减会让预算变成非正数。因此这里只把输出预留作为 headroom，
// 并保留 DSH 的比例。当调用方窗口更大时，比例上限本来就会占优。
func contextPromptBudget(agent *AgentInstance, policy ContextPressurePolicy) int {
	if agent == nil || agent.ContextWindow <= 0 {
		return 0
	}
	ratio := policy.ThresholdRatio
	if ratio <= 0 || ratio > 1 {
		ratio = DefaultContextPressurePolicy().ThresholdRatio
	}
	maxOutput := agent.MaxTokens
	if maxOutput < 0 {
		maxOutput = 0
	}
	bound := int(float64(agent.ContextWindow) * ratio)
	effective := agent.ContextWindow - maxOutput
	if effective < 1 {
		effective = 1
	}
	if effective < bound {
		bound = effective
	}
	if bound < 1 {
		bound = 1
	}
	return bound
}

// calibratedPromptTokens 估算下一次请求的 prompt 大小，
// 并用最近一次 provider 锚点校正启发式值。
func calibratedPromptTokens(
	ts *turnState,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
) int {
	estimated := estimateRequestTokens(messages, toolDefs)
	ratio := ts.usageAnchorSnapshot().correctionRatio()
	corrected := int(float64(estimated) * ratio)
	if corrected < estimated {
		// 启发式值对工具 schema/CJK 而言是下界；不能让校正把请求算得比实测还小。
		corrected = estimated
	}
	return corrected
}

// contextPressureNotice 渲染模型可见的接近上限通知。
func contextPressureNotice(used, budget int, ratio float64) string {
	percent := int(ratio*100 + 0.5)
	var b strings.Builder
	b.WriteString("<context_pressure>\n")
	fmt.Fprintf(&b, "上下文使用约 %d tokens（预算约 %d，已用 %d%%）。\n", used, budget, percent)
	b.WriteString("优先完成当前子任务，把关键结论写入任务状态或文件；避免重复读取大文件；继续前先总结已完成内容。\n")
	b.WriteString("</context_pressure>")
	return b.String()
}

// maybeContextPressureNotice 在校正后的压力首次跨过配置区间、
// 且本轮尚未提示过该区间时返回接近上限通知。
func (ts *turnState) maybeContextPressureNotice(
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
) *providers.Message {
	if ts == nil || ts.agent == nil {
		return nil
	}
	budget := contextPromptBudget(ts.agent, DefaultContextPressurePolicy())
	if budget <= 0 {
		return nil
	}
	used := calibratedPromptTokens(ts, messages, toolDefs)
	if used <= 0 {
		return nil
	}
	ratio := float64(used) / float64(budget)
	band := ""
	for _, threshold := range contextPressureNoticeRatios {
		if ratio >= threshold {
			band = fmt.Sprintf("%.2f", threshold)
		}
	}
	if band == "" {
		return nil
	}
	if ts.contextPressureNoticeDigest() == band {
		return nil
	}
	ts.setContextPressureNoticeDigest(band)
	text := contextPressureNotice(used, budget, ratio)
	return &providers.Message{
		Role:    "user",
		Content: text,
		Context: &protocoltypes.ContextMeta{
			Kind:   protocoltypes.ContextKindContextPressure,
			Form:   protocoltypes.ContextFormReminder,
			Digest: band,
		},
	}
}

func (ts *turnState) contextPressureNoticeDigest() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.contextPressureNotice
}

func (ts *turnState) setContextPressureNoticeDigest(digest string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.contextPressureNotice = digest
}
