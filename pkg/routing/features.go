package routing

import (
	"strings"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
)

// lookbackWindow 是扫描工具调用时检查的近期历史条数。
// 六条大致覆盖一轮完整的工具使用往返（user → assistant+tool_call → tool_result → assistant）。
const lookbackWindow = 6

// Features 保存从消息及其会话上下文中提取的结构性信号。
// 每个维度在设计上都与语言无关 —— 不对自然语言内容做
// 关键词或模式匹配。这样可确保对所有语言环境的路由行为保持一致。
type Features struct {
	// TokenEstimate 是 token 数量的近似值。
	// CJK 字符每个计为 1 个 token；非 CJK 字符每个计为 0.25 个 token。
	// 这样既能避免调用 API，又能对所有文字系统给出准确的估算。
	TokenEstimate int

	// CodeBlockCount 是消息中围栏代码块（成对的 ```）的数量。
	// 编码任务几乎总是需要重量级模型。
	CodeBlockCount int

	// RecentToolCalls 是最近 lookbackWindow 条历史记录中 tool_call 消息的数量。
	// 密度高表明正处于活跃的 agent 工作流中。
	RecentToolCalls int

	// ConversationDepth 是会话历史中的消息总数。
	// 较深的会话往往承载着多轮累积起来的隐含复杂性。
	ConversationDepth int

	// 当消息看起来包含媒体（图片、音频、视频）时，HasAttachments 为 true。
	// 多模态输入需要具备视觉能力的重量级模型。
	HasAttachments bool
}

// ExtractFeatures 计算一条消息的结构性特征向量。
// 它是纯函数，没有副作用，除返回的结构体外不产生任何内存分配。
func ExtractFeatures(msg string, history []providers.Message) Features {
	return Features{
		TokenEstimate:     estimateTokens(msg),
		CodeBlockCount:    countCodeBlocks(msg),
		RecentToolCalls:   countRecentToolCalls(history),
		ConversationDepth: len(history),
		HasAttachments:    hasAttachments(msg),
	}
}

// estimateTokens 返回一个可同时处理 CJK 与拉丁文本的 token 数近似值。
// CJK 字符（U+2E80–U+9FFF、U+F900–U+FAFF、U+AC00–U+D7AF）大致各映射为
// 一个 token，而非 CJK 字符平均约为 0.25 token/字符（英文约为每 token 4 个字符）。
// 这样拆分计数可以避免用固定的 rune_count/3 估算中文、日文和韩文文本时
// 产生的 3 倍低估。
func estimateTokens(msg string) int {
	total := utf8.RuneCountInString(msg)
	if total == 0 {
		return 0
	}
	cjk := 0
	for _, r := range msg {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF || r >= 0xAC00 && r <= 0xD7AF {
			cjk++
		}
	}
	return cjk + (total-cjk)/4
}

// countCodeBlocks 统计完整的围栏代码块数量。
// 每个 ``` 分隔符使计数器加一；成对的分隔符构成一个代码块。
// 未闭合的起始围栏（计数为奇数）按零个完整代码块处理，
// 因为它可能只是一个行内代码片段或笔误。
func countCodeBlocks(msg string) int {
	n := strings.Count(msg, "```")
	return n / 2
}

// countRecentToolCalls 统计历史记录中最近 lookbackWindow 条里带工具调用的消息数。
// 它检查 ToolCalls 字段而不是解析内容字符串，
// 因此对任何消息格式都稳健。
func countRecentToolCalls(history []providers.Message) int {
	start := len(history) - lookbackWindow
	if start < 0 {
		start = 0
	}

	count := 0
	for _, msg := range history[start:] {
		if len(msg.ToolCalls) > 0 {
			count += len(msg.ToolCalls)
		}
	}
	return count
}

// 当消息内容包含内嵌媒体时，hasAttachments 返回 true。
// 它会检查 base64 data URI（data:image/、data:audio/、data:video/）以及
// 常见的图片/音频 URL 扩展名。这里刻意采用保守策略 ——
// 漏判（未识别出附件）只会让路由回退到主模型，不会造成其他影响。
func hasAttachments(msg string) bool {
	lower := strings.ToLower(msg)

	// 直接内嵌在消息中的 base64 data URI
	if strings.Contains(lower, "data:image/") ||
		strings.Contains(lower, "data:audio/") ||
		strings.Contains(lower, "data:video/") {
		return true
	}

	// URL 或文件引用中常见的图片/音频扩展名
	mediaExts := []string{
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp",
		".mp3", ".wav", ".ogg", ".m4a", ".flac",
		".mp4", ".avi", ".mov", ".webm",
	}
	for _, ext := range mediaExts {
		if strings.Contains(lower, ext) {
			return true
		}
	}

	return false
}
