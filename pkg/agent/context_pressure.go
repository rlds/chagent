// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"math"

	"github.com/rlds/chagent/pkg/providers"
)

// ContextPressurePolicy 是 DSH compaction-basic 阈值策略的纯函数、无副作用 Go 移植
// （packages/compaction/compaction-basic/src/config.ts:140-213）：
//
//	messageBudget  = contextWindow - reservedOutput
//	pressureBudget = messageBudget - headroomTokens
//	threshold      = floor(min(contextWindow*thresholdRatio, pressureBudget))
//	retain         = floor(messageBudget*retainRatio)
//
// 迁移方向：chagent 当前的压缩触发仍是 isOverContextBudget（context_budget.go），
// 它把整个请求（消息 + 工具定义 + 输出预留）与上下文窗口比较，因此在窗口 100% 时
// 才触发。本策略是 DSH 兼容的目标模型：调用方可以逐步采用它做主动压力压缩，
// 而不改变现有默认行为；isOverContextBudget 继续作为 provider 侧上下文溢出错误的
// 硬性安全网。
type ContextPressurePolicy struct {
	// ThresholdRatio 是应触发主动压缩的上下文窗口比例。
	// 超出 (0,1] 的值回退到默认值。
	ThresholdRatio float64
	// RetainRatio 是压缩后原样保留的消息预算比例（DSH 的 retainRatio）。
	// 钳制到 [0,1]。
	RetainRatio float64
	// HeadroomTokens 是输出预留之外额外保留的余量。
	// 负值按零处理。
	HeadroomTokens int
}

// DefaultContextPressurePolicy 返回 DSH 的默认压力策略：
// 窗口 80% 时触发压缩，原样保留至少 16% 消息预算，
// 并在输出预留之上保留 65536 token 余量。
func DefaultContextPressurePolicy() ContextPressurePolicy {
	return ContextPressurePolicy{
		ThresholdRatio: 0.8,
		RetainRatio:    0.16,
		HeadroomTokens: 65536,
	}
}

// Threshold 返回给定上下文窗口与输出预留下应触发主动压缩的 prompt token 数。
//
// 结果同时受窗口比例和“输出预留 + headroom 之后剩余容量”限制，与 DSH 一致。
// 下界为 1 token，保证调用方总能得到可用的触发值（否则非正预算会让压力无法被
// 检测）；调用方仍以 isOverContextBudget 作为最终守卫。
func (p ContextPressurePolicy) Threshold(contextWindow, maxOutput int) int {
	if contextWindow <= 0 {
		return 0
	}
	if maxOutput < 0 {
		maxOutput = 0
	}

	ratio := p.ThresholdRatio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		ratio = DefaultContextPressurePolicy().ThresholdRatio
	}
	if ratio > 1 {
		ratio = 1
	}

	headroom := p.HeadroomTokens
	if headroom < 0 {
		headroom = 0
	}

	ratioBound := int(math.Floor(float64(contextWindow) * ratio))
	pressureBudget := contextWindow - maxOutput - headroom

	threshold := ratioBound
	if pressureBudget < threshold {
		threshold = pressureBudget
	}
	if threshold < 1 {
		return 1
	}
	if threshold > contextWindow {
		threshold = contextWindow
	}
	return threshold
}

// RetainTokens 返回压缩必须原样保留的最近 prompt token 数（DSH 的 retainTokens）。
// 它用 RetainRatio 缩放消息预算（窗口减去输出预留）。
//
// DSH 在配置加载时校验 retainTokens < thresholdTokens；本纯函数不做校验，
// 调用方必须自行维持该不变量（默认策略在任何现实窗口下都满足）。
func (p ContextPressurePolicy) RetainTokens(contextWindow, maxOutput int) int {
	if contextWindow <= 0 {
		return 0
	}
	if maxOutput < 0 {
		maxOutput = 0
	}
	messageBudget := contextWindow - maxOutput
	if messageBudget <= 0 {
		return 0
	}

	ratio := p.RetainRatio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	retain := int(math.Floor(float64(messageBudget) * ratio))
	if retain < 0 {
		return 0
	}
	return retain
}

// Pressure 是对照上下文窗口的一次纯压力测量。
type Pressure struct {
	// UsedTokens 是组装后请求的估算大小：system prompt、
	// 工具定义以及将要发送的每条消息。
	UsedTokens int
	// Threshold 是应触发压缩的 token 数。
	Threshold int
	// Over 表示 UsedTokens 是否已达到 Threshold。与 DSH 一致，
	// 压力压缩在“>= 阈值”时触发（DSH 仅在 totalTokens < thresholdTokens 时跳过）。
	Over bool
	// Pct 是 UsedTokens 占上下文窗口的百分比（0-100+）。
	Pct float64
}

// Pressure 在不改动任何输入的前提下，用策略测量候选请求。
// systemTokens 表示 system prompt，toolTokens 表示工具定义，
// messages 表示将要发送的对话内容。
func (p ContextPressurePolicy) Pressure(
	messages []providers.Message,
	systemTokens, toolTokens, contextWindow, maxOutput int,
) Pressure {
	used := systemTokens + toolTokens
	for _, msg := range messages {
		used += EstimateMessageTokens(msg)
	}

	threshold := p.Threshold(contextWindow, maxOutput)
	pressure := Pressure{
		UsedTokens: used,
		Threshold:  threshold,
		Over:       threshold > 0 && used >= threshold,
	}
	if contextWindow > 0 {
		pressure.Pct = float64(used) * 100 / float64(contextWindow)
	}
	return pressure
}
