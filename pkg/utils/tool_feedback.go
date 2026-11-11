package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const ToolFeedbackContinuationHint = "继续当前任务。"

func FormatArgsJSON(args map[string]any, prettyPrint, disableEscapeHTML bool) string {
	// 将 nil 归一化为空 map，以保证输出一致
	if args == nil {
		args = map[string]any{}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if prettyPrint {
		enc.SetIndent("", "  ")
	}
	if disableEscapeHTML {
		enc.SetEscapeHTML(false)
	}
	if err := enc.Encode(args); err != nil {
		// 回退到 fmt.Sprintf，以便仍能看到有问题的参数
		return fmt.Sprintf("%v", args)
	}
	return strings.TrimSpace(buf.String())
}

// FormatToolFeedbackMessage 为聊天渠道渲染工具反馈消息。
// 它将工具名保留在第一行以便动画显示，正文中可同时包含
// 面向人的说明和序列化后的工具参数。
func FormatToolFeedbackMessage(toolName, explanation, argsPreview string) string {
	toolName = strings.TrimSpace(toolName)
	explanation = strings.TrimSpace(explanation)
	argsPreview = strings.TrimSpace(argsPreview)

	bodyLines := make([]string, 0, 2)
	if explanation != "" {
		bodyLines = append(bodyLines, explanation)
	}
	if argsPreview != "" {
		bodyLines = append(bodyLines, "```json\n"+argsPreview+"\n```")
	}
	body := strings.Join(bodyLines, "\n")

	if toolName == "" {
		return body
	}
	if body == "" {
		return fmt.Sprintf("\U0001f527 `%s`", toolName)
	}

	return fmt.Sprintf("\U0001f527 `%s`\n%s", toolName, body)
}

// FitToolFeedbackMessage 使工具反馈保持为单条外发消息。
// 它尽可能保留第一行，并截断说明正文，
// 而不是让消息被拆分成多个分片。
func FitToolFeedbackMessage(content string, maxLen int) string {
	content = strings.TrimSpace(content)
	if content == "" || maxLen <= 0 {
		return ""
	}
	if len([]rune(content)) <= maxLen {
		return content
	}

	firstLine, rest, hasRest := strings.Cut(content, "\n")
	firstLine = strings.TrimSpace(firstLine)
	rest = strings.TrimSpace(rest)

	if !hasRest || rest == "" {
		return Truncate(firstLine, maxLen)
	}

	if len([]rune(firstLine)) >= maxLen {
		return Truncate(firstLine, maxLen)
	}

	remaining := maxLen - len([]rune(firstLine)) - 1
	if remaining <= 0 {
		return Truncate(firstLine, maxLen)
	}

	return firstLine + "\n" + Truncate(rest, remaining)
}
