// Chagent - 超轻量级个人 AI 智能体
// 灵感来源于 nanobot 并基于其实现：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package utils

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
)

// CalculateDefaultMaxContextRunes 根据模型的上下文窗口计算默认上下文上限。
// 策略：取上下文窗口的 75%，再换算为 rune 估算值。
//
// Token 与 rune 的换算比例（保守估算）：
//   - 英文：每 token 约 4 个字符
//   - 中文：每 token 约 1.5-2 个字符
//   - 中英混合：每 token 约 3 个字符（此处出于安全考虑采用该值）
func CalculateDefaultMaxContextRunes(contextWindow int) int {
	if contextWindow <= 0 {
		// 上下文窗口未知时的保守回退值
		return 8000 // 约 2000 个 token
	}

	// 使用上下文窗口的 75%，预留余量
	targetTokens := int(float64(contextWindow) * 0.75)

	// 按保守比例将 token 换算为 rune
	const avgCharsPerToken = 3
	return targetTokens * avgCharsPerToken
}

// ResolveMaxContextRunes 确定最终使用的 MaxContextRunes 值。
// 优先级：显式配置 > 自动计算 > 保守默认值
func ResolveMaxContextRunes(configValue, contextWindow int) int {
	switch {
	case configValue > 0:
		// 已显式配置，直接使用
		return configValue
	case configValue == -1:
		// 已显式禁用
		return -1
	default:
		// 为 0 或未设置：自动计算
		return CalculateDefaultMaxContextRunes(contextWindow)
	}
}

// MeasureContextRunes 计算消息列表的 rune 总数。
// 包括内容、推理内容以及对工具调用的估算。
func MeasureContextRunes(messages []providers.Message) int {
	totalRunes := 0
	for _, msg := range messages {
		totalRunes += utf8.RuneCountInString(msg.Content)
		totalRunes += utf8.RuneCountInString(msg.ReasoningContent)

		// 工具调用：序列化为 JSON 后计数
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				totalRunes += utf8.RuneCountInString(tc.Name)
				// 参数：序列化后计数
				if argsJSON, err := json.Marshal(tc.Arguments); err == nil {
					totalRunes += utf8.RuneCount(argsJSON)
				} else {
					// 序列化失败时的回退估算值
					totalRunes += 100
				}
			}
		}

		// 工具调用 ID
		totalRunes += utf8.RuneCountInString(msg.ToolCallID)
	}
	return totalRunes
}

// TruncateContextSmart 智能截断消息历史，使其不超过 maxRunes。
//
// 策略：
//  1. 始终保留系统消息（它们定义了智能体的行为）
//  2. 保留最近的消息（它们包含当前上下文）
//  3. 必要时丢弃较早的中间消息
//  4. 插入截断提示，以便告知 LLM
//
// 返回截断后的消息列表。
func TruncateContextSmart(messages []providers.Message, maxRunes int) []providers.Message {
	if len(messages) == 0 {
		return messages
	}

	// 将系统消息与其他消息分开
	var systemMsgs []providers.Message
	var otherMsgs []providers.Message

	for _, msg := range messages {
		if msg.Role == "system" {
			systemMsgs = append(systemMsgs, msg)
		} else {
			otherMsgs = append(otherMsgs, msg)
		}
	}

	// 计算系统消息大小
	systemRunes := 0
	for _, msg := range systemMsgs {
		systemRunes += utf8.RuneCountInString(msg.Content)
		systemRunes += utf8.RuneCountInString(msg.ReasoningContent)
	}

	// 为截断提示预留空间（估算约 80 个 rune）
	const truncationNoticeEstimate = 80

	// 为其他消息分配剩余空间
	remainingRunes := maxRunes - systemRunes - truncationNoticeEstimate
	if remainingRunes <= 0 {
		// 系统消息已超出限制——只返回系统消息
		return systemMsgs
	}

	// 逆序收集最近的消息，直到达到上限
	var keptMsgs []providers.Message
	currentRunes := 0

	for i := len(otherMsgs) - 1; i >= 0; i-- {
		msg := otherMsgs[i]
		msgRunes := utf8.RuneCountInString(msg.Content) +
			utf8.RuneCountInString(msg.ReasoningContent)

		// 估算工具调用大小
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				msgRunes += utf8.RuneCountInString(tc.Name)
				if argsJSON, err := json.Marshal(tc.Arguments); err == nil {
					msgRunes += utf8.RuneCount(argsJSON)
				} else {
					msgRunes += 100
				}
			}
		}
		msgRunes += utf8.RuneCountInString(msg.ToolCallID)

		if currentRunes+msgRunes > remainingRunes {
			// 将超出限制，停止收集
			break
		}

		// 前插以保持时间顺序
		keptMsgs = append([]providers.Message{msg}, keptMsgs...)
		currentRunes += msgRunes
	}

	// 如果有消息被丢弃，则添加截断提示
	result := systemMsgs
	if len(keptMsgs) < len(otherMsgs) {
		droppedCount := len(otherMsgs) - len(keptMsgs)
		truncationNotice := providers.Message{
			Role: "system",
			Content: fmt.Sprintf(
				"[上下文已截断：为保持在上下文限制内，省略了较早的 %d 条消息]",
				droppedCount,
			),
		}
		result = append(result, truncationNotice)
	}

	result = append(result, keptMsgs...)
	return result
}
