// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package channels

import (
	"strings"
)

// MessageSplitMarker 是用于把一条消息拆分为多条出站消息的分隔符。
// 当配置中启用 SplitOnMarker 时，Manager 会按该标记拆分消息，
// 并把每一部分作为独立消息发送。
const MessageSplitMarker = "<|[SPLIT]|>"

// SplitByMarker 按 MessageSplitMarker 拆分消息并返回各部分。
// 空的部分（包括由连续标记产生的空部分）会被过滤掉。
// 若未找到标记，则返回仅包含原始内容的单元素切片。
func SplitByMarker(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, MessageSplitMarker)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return []string{content}
	}
	return result
}
