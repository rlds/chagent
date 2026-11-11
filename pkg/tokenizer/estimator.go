package tokenizer

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
)

// EstimateMessageTokens 估算单条消息的 token 数量，
// 统计范围包括 Content、ReasoningContent、ToolCalls 参数、ToolCallID
// 元数据以及 Media 条目。
//
// 字符启发式估算会区分 CJK：ASCII 文本平均约 2.5 个字符/token，
// 而在支持中文的 tokenizer（Qwen/DeepSeek/GLM）上，CJK 文本平均约为
// 1.3 个字符/token。若统一采用同一个比例，会低估中文上下文，
// 导致预算检查（压缩、历史裁剪）触发过晚，进而引发
// 服务提供方侧的上下文溢出错误。
func EstimateMessageTokens(msg providers.Message) int {
	contentChars := utf8.RuneCountInString(msg.Content)

	// SystemParts 是用于缓存感知适配器的结构化系统块。
	// 它们携带与 Content 相同的内容，只是拆分成多个块。
	// 这里将它们视为一种替代表示来估算，而不是叠加计算。
	systemPartsChars := 0
	if len(msg.SystemParts) > 0 {
		for _, part := range msg.SystemParts {
			systemPartsChars += utf8.RuneCountInString(part.Text)
		}
		// 每个分块为 JSON 结构（type、text、cache_control）产生的额外开销。
		const perPartOverhead = 20
		systemPartsChars += len(msg.SystemParts) * perPartOverhead
	}

	// 取两种表示中较大的一个，以保持估算保守。
	chars := contentChars
	cjkChars := CJKRuneCount(msg.Content)
	if systemPartsChars > chars {
		chars = systemPartsChars
		cjkChars = 0
		for _, part := range msg.SystemParts {
			cjkChars += CJKRuneCount(part.Text)
		}
		if cjkChars > chars {
			cjkChars = chars
		}
	}

	chars += utf8.RuneCountInString(msg.ReasoningContent)
	cjkChars += CJKRuneCount(msg.ReasoningContent)

	for _, tc := range msg.ToolCalls {
		chars += len(tc.ID) + len(tc.Type)
		if tc.Function != nil {
			// 统计函数名与参数（这是大多数服务提供方的线上格式）。
			// tc.Name 与 tc.Function.Name 相同，只统计一次以避免重复计数。
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		} else {
			// 兜底：部分服务提供方的格式使用顶层 Name 而不带 Function。
			chars += len(tc.Name)
		}
	}

	if msg.ToolCallID != "" {
		chars += len(msg.ToolCallID)
	}

	// 每条消息在角色标签、JSON 结构和分隔符上的额外开销。
	const messageOverhead = 12
	chars += messageOverhead

	tokens := weightedTokenEstimate(chars, cjkChars)

	// 媒体条目（图片、文件）会由服务提供方适配器序列化为
	// multipart 或 image_url 载荷。这里直接为每个条目加上固定的 token
	// 估算值（不经过字符启发式计算），因为实际开销取决于
	// 分辨率和各服务提供方特有的图片分词方式。
	const mediaTokensPerItem = 256
	tokens += len(msg.Media) * mediaTokensPerItem

	return tokens
}

// EstimateToolDefsTokens 估算工具定义在 LLM 请求中
// 占用的总 token 开销。
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
	if len(defs) == 0 {
		return 0
	}

	totalChars := 0
	for _, d := range defs {
		totalChars += len(d.Function.Name) + len(d.Function.Description)

		if d.Function.Parameters != nil {
			if paramJSON, err := json.Marshal(d.Function.Parameters); err == nil {
				totalChars += len(paramJSON)
			}
		}

		// 每个工具的额外开销：type 字段、JSON 结构、分隔符。
		totalChars += 20
	}

	return totalChars * 2 / 5
}

// CJKRuneCount 返回 s 中 CJK 字符的个数：汉字、假名、谚文、
// CJK 标点以及全角形式。用于对中文占比较高的上下文
// 加权 token 估算。
func CJKRuneCount(s string) int {
	count := 0
	for _, r := range s {
		if isCJKRun(r) {
			count++
		}
	}
	return count
}

func isCJKRun(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK 部首、假名、CJK 符号/标点、统一汉字
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 谚文音节
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容汉字
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角形式（，！？ 等）
		return true
	case r >= 0x20000 && r <= 0x3FFFF: // CJK 扩展 B 区及以后
		return true
	default:
		return false
	}
}

// weightedTokenEstimate 将各类字符的权重混合为一个 token 估算值：
// ASCII 沿用历史的 0.4 token/字符（2.5 字符/token），使现有英文
// 预算保持不变；CJK 字符则按 0.75 token/字符估算。
func weightedTokenEstimate(chars, cjkChars int) int {
	if cjkChars > chars {
		// 防御性处理：CJK 字符数应是总字符数的子集。
		cjkChars = chars
	}
	ascii := chars - cjkChars
	return ascii*2/5 + cjkChars*3/4
}
