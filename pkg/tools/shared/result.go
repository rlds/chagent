package toolshared

import (
	"encoding/json"
	"strings"

	"github.com/rlds/chagent/pkg/providers"
)

const (
	HandledToolLLMNote   = "请求的输出已在当前对话中交付给用户。不要再调用 send_file 或任何其他交付工具。如需回复，只需给出简短确认。"
	ArtifactPathsLLMNote = "如需发送给用户，请使用 `send_file` 并传入以下路径之一；如需保存到工作区，请使用文件工具或 exec 工具。"
)

// ToolResult 表示工具执行的结构化返回值。
// 它为不同类型的返回结果提供清晰语义，并支持
// 异步操作、面向用户的消息以及错误处理。
type ToolResult struct {
	// ForLLM 是发送给 LLM 作为上下文的内容。
	// 所有结果都必需。
	ForLLM string `json:"for_llm"`

	// ForUser 是直接发送给用户的内容。
	// 如果为空，则不发送任何用户消息。
	// Silent=true 会覆盖该字段。
	ForUser string `json:"for_user,omitempty"`

	// Silent 抑制向用户发送任何消息。
	// 当为 true 时，即使设置了 ForUser 也会被忽略。
	Silent bool `json:"silent"`

	// IsError 表示工具执行是否失败。
	// 当为 true 时，该结果应被当作错误处理。
	IsError bool `json:"is_error"`

	// Async 表示工具是否正在异步运行。
	// 当为 true 时，工具将在稍后完成并通过回调通知。
	Async bool `json:"async"`

	// Err 是底层错误（不会被 JSON 序列化）。
	// 用于内部错误处理和日志记录。
	Err error `json:"-"`

	// Media 包含该工具产生的媒体存储引用。
	// 当非空时，agent 会将它们发布为 OutboundMediaMessage。
	Media []string `json:"media,omitempty"`

	// Messages 保存执行后的临时会话历史。
	// 仅由 SubTurn 执行填充；供 evaluator_optimizer
	// 在评估迭代之间承载有状态的 worker 上下文。
	Messages []providers.Message `json:"-"`

	// ArtifactTags 以结构化形式把本地工件路径暴露给 LLM，
	// 例如 "[file:/tmp/example.png]"。当工具产出了可复用的本地工件
	// 但尚未交付给用户时会使用该字段。
	ArtifactTags []string `json:"artifact_tags,omitempty"`

	// ResponseHandled 表示本次工具执行已在渠道/输出层面满足了
	// 用户的请求，因此 agent 循环可以停止，无需再产生后续助手回复。
	ResponseHandled bool `json:"response_handled,omitempty"`
}

// ContentForLLM 返回在工具调用之后应追加到对话中的规范化文本内容。
// 当 ForLLM 为空时，错误信息回退到 Err。
func (tr *ToolResult) ContentForLLM() string {
	if tr == nil {
		return ""
	}
	content := tr.ForLLM
	if content == "" && tr.Err != nil {
		content = tr.Err.Error()
	}
	if tr.ResponseHandled {
		if content == "" {
			return HandledToolLLMNote
		}
		if !strings.Contains(content, HandledToolLLMNote) {
			content += "\n" + HandledToolLLMNote
		}
	}
	if len(tr.ArtifactTags) > 0 {
		artifactNote := "本地工件路径：" + strings.Join(tr.ArtifactTags, " ") + "\n" + ArtifactPathsLLMNote
		if content == "" {
			content = artifactNote
		} else if !strings.Contains(content, artifactNote) {
			content += "\n" + artifactNote
		}
	}
	if content != "" {
		return content
	}
	return ""
}

// NewToolResult 创建一个携带 LLM 内容的基础 ToolResult。
// 当你需要一个采用默认行为的简单结果时使用它。
//
// 示例：
//
//	result := NewToolResult("File updated successfully")
func NewToolResult(forLLM string) *ToolResult {
	return &ToolResult{
		ForLLM: forLLM,
	}
}

// SilentResult 创建一个静默的 ToolResult（不发送用户消息）。
// 内容只会发送给 LLM 作为上下文。
//
// 用于那些不应打扰用户的操作，例如：
// - 文件读写
// - 状态更新
// - 后台操作
//
// 示例：
//
//	result := SilentResult("Config file saved")
func SilentResult(forLLM string) *ToolResult {
	return &ToolResult{
		ForLLM:  forLLM,
		Silent:  true,
		IsError: false,
		Async:   false,
	}
}

// AsyncResult 为异步操作创建一个 ToolResult。
// 任务将在后台运行并在稍后完成。
//
// 用于长时间运行的操作，例如：
// - 派生子 agent
// - 后台处理
// - 带回调的外部 API 调用
//
// 示例：
//
//	result := AsyncResult("Subagent spawned, will report back")
func AsyncResult(forLLM string) *ToolResult {
	return &ToolResult{
		ForLLM:  forLLM,
		Silent:  false,
		IsError: false,
		Async:   true,
	}
}

// ErrorResult 创建一个表示错误的 ToolResult。
// 会设置 IsError=true 并包含错误消息。
//
// 示例：
//
//	result := ErrorResult("Failed to connect to database: connection refused")
func ErrorResult(message string) *ToolResult {
	return &ToolResult{
		ForLLM:  message,
		Silent:  false,
		IsError: true,
		Async:   false,
	}
}

// UserResult 创建一个同时面向 LLM 和用户的 ToolResult。
// ForLLM 与 ForUser 会被设置为相同的内容。
//
// 用于用户需要直接看到结果的情况：
// - 命令执行输出
// - 抓取到的网页内容
// - 查询结果
//
// 示例：
//
//	result := UserResult("Total files found: 42")
func UserResult(content string) *ToolResult {
	return &ToolResult{
		ForLLM:  content,
		ForUser: content,
		Silent:  false,
		IsError: false,
		Async:   false,
	}
}

// MediaResult 创建一个面向用户、带有媒体引用的 ToolResult。
// agent 会将这些引用发布为 OutboundMediaMessage。
//
// 示例：
//
//	result := MediaResult("Image generated successfully", []string{"media://abc123"})
func MediaResult(forLLM string, mediaRefs []string) *ToolResult {
	return &ToolResult{
		ForLLM: forLLM,
		Media:  mediaRefs,
	}
}

// MarshalJSON 实现自定义 JSON 序列化。
// Err 字段通过 json:"-" 标签被排除在 JSON 输出之外。
func (tr *ToolResult) MarshalJSON() ([]byte, error) {
	type Alias ToolResult
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(tr),
	})
}

// WithError 设置 Err 字段并返回该结果以支持链式调用。
// 这样既保留了用于日志记录的错误，又不让它出现在 JSON 中。
//
// 示例：
//
//	result := ErrorResult("Operation failed").WithError(err)
func (tr *ToolResult) WithError(err error) *ToolResult {
	tr.Err = err
	return tr
}

// WithResponseHandled 把工具结果标记为已交付给用户。
func (tr *ToolResult) WithResponseHandled() *ToolResult {
	tr.ResponseHandled = true
	return tr
}
