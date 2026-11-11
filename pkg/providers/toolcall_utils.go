// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package providers

import "encoding/json"

// NormalizeToolCall 规范化 ToolCall，确保所有字段都被正确填充。
// 它处理 Name/Arguments 可能位于不同位置（顶层或 Function 内）的情况，
// 并确保两者以一致的方式被填充。
func NormalizeToolCall(tc ToolCall) ToolCall {
	normalized := tc

	if normalized.ThoughtSignature == "" &&
		normalized.ExtraContent != nil &&
		normalized.ExtraContent.Google != nil {
		normalized.ThoughtSignature = normalized.ExtraContent.Google.ThoughtSignature
	}

	// 若 Name 未设置，则用 Function 中的值填充
	if normalized.Name == "" && normalized.Function != nil {
		normalized.Name = normalized.Function.Name
	}

	// 确保 Arguments 不为 nil
	if normalized.Arguments == nil {
		normalized.Arguments = map[string]any{}
	}

	// 若尚未设置，则从 Function.Arguments 解析 Arguments
	if len(normalized.Arguments) == 0 && normalized.Function != nil && normalized.Function.Arguments != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(normalized.Function.Arguments), &parsed); err == nil && parsed != nil {
			normalized.Arguments = parsed
		}
	}

	// 确保 Function 中各字段以一致的值填充
	argsJSON, _ := json.Marshal(normalized.Arguments)
	if normalized.Function == nil {
		normalized.Function = &FunctionCall{
			Name:             normalized.Name,
			Arguments:        string(argsJSON),
			ThoughtSignature: normalized.ThoughtSignature,
		}
	} else {
		if normalized.Function.Name == "" {
			normalized.Function.Name = normalized.Name
		}
		if normalized.Name == "" {
			normalized.Name = normalized.Function.Name
		}
		if normalized.Function.Arguments == "" {
			normalized.Function.Arguments = string(argsJSON)
		}
		if normalized.Function.ThoughtSignature == "" {
			normalized.Function.ThoughtSignature = normalized.ThoughtSignature
		}
		if normalized.ThoughtSignature == "" {
			normalized.ThoughtSignature = normalized.Function.ThoughtSignature
		}
	}

	return normalized
}
