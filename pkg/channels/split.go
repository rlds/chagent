package channels

import (
	"strings"
)

// SplitMessage 将长消息拆分为多个分块，并保持代码块的完整性。
// maxLen 参数以 rune（Unicode 字符）为单位，而非字节。
// 该函数会预留一段缓冲区（maxLen 的 10%，最少 50 个字符），为补全代码块留出空间，
// 但在需要时可以扩展到 maxLen。
// 调用 SplitMessage 时传入完整文本内容和单条消息允许的最大长度；
// 它返回若干消息分块，每块都满足 maxLen，并避免拆分围栏代码块。
func SplitMessage(content string, maxLen int) []string {
	if maxLen <= 0 {
		if content == "" {
			return nil
		}
		return []string{content}
	}

	runes := []rune(content)
	totalLen := len(runes)
	var messages []string

	// 动态缓冲区：maxLen 的 10%，但在可能时至少 50 个字符
	codeBlockBuffer := max(maxLen/10, 50)
	if codeBlockBuffer > maxLen/2 {
		codeBlockBuffer = maxLen / 2
	}

	start := 0
	for start < totalLen {
		remaining := totalLen - start
		if remaining <= maxLen {
			messages = append(messages, string(runes[start:totalLen]))
			break
		}

		// 有效切分点：maxLen 减去缓冲区，为代码块留出空间
		effectiveLimit := max(maxLen-codeBlockBuffer, maxLen/2)

		end := start + effectiveLimit

		// 在有效限制范围内寻找自然切分点
		msgEnd := findLastNewlineInRange(runes, start, end, 200)
		if msgEnd <= start {
			msgEnd = findLastSpaceInRange(runes, start, end, 100)
		}
		if msgEnd <= start {
			msgEnd = end
		}

		// 检查这样切分是否会以未闭合的代码块结尾
		unclosedIdx := findLastUnclosedCodeBlockInRange(runes, start, msgEnd)

		if unclosedIdx >= 0 {
			// 消息将以未闭合的代码块结尾
			// 尝试扩展到 maxLen，以包含闭合的 ```
			if totalLen > msgEnd {
				closingIdx := findNextClosingCodeBlockInRange(runes, msgEnd, totalLen)
				if closingIdx > 0 && closingIdx-start <= maxLen {
					// 扩展以包含闭合的 ```
					msgEnd = closingIdx
				} else {
					// 代码块太长，无法放入单个分块，或缺少闭合围栏。
					// 尝试在代码块内部拆分，通过注入闭合围栏和重新开启围栏来实现。
					headerEnd := findNewlineFrom(runes, unclosedIdx)
					var header string
					if headerEnd == -1 {
						header = strings.TrimSpace(string(runes[unclosedIdx : unclosedIdx+3]))
					} else {
						header = strings.TrimSpace(string(runes[unclosedIdx:headerEnd]))
					}
					headerEndIdx := unclosedIdx + len([]rune(header))
					if headerEnd != -1 {
						headerEndIdx = headerEnd
					}

					// 如果头部之后还有足够多的内容，就在内部拆分
					if msgEnd > headerEndIdx+20 {
						// 寻找更接近 maxLen 的更优切分点
						innerLimit := min(
							// 为 "\n```" 预留空间
							start+maxLen-5, totalLen)
						betterEnd := findLastNewlineInRange(runes, start, innerLimit, 200)
						if betterEnd > headerEndIdx {
							msgEnd = betterEnd
						} else {
							msgEnd = innerLimit
						}
						chunk := strings.TrimRight(string(runes[start:msgEnd]), " \t\n\r") + "\n```"
						messages = append(messages, chunk)
						remaining := strings.TrimSpace(header + "\n" + string(runes[msgEnd:totalLen]))
						// 用重建后的剩余内容替换 runes 的尾部
						runes = []rune(remaining)
						totalLen = len(runes)
						start = 0
						continue
					}

					// 否则，尝试在代码块开始之前拆分
					newEnd := findLastNewlineInRange(runes, start, unclosedIdx, 200)
					if newEnd <= start {
						newEnd = findLastSpaceInRange(runes, start, unclosedIdx, 100)
					}
					if newEnd > start {
						msgEnd = newEnd
					} else {
						// 如果无法在之前拆分，就只能在内部拆分（最后手段）
						if unclosedIdx-start > 20 {
							msgEnd = unclosedIdx
						} else {
							splitAt := min(start+maxLen-5, totalLen)
							chunk := strings.TrimRight(string(runes[start:splitAt]), " \t\n\r") + "\n```"
							messages = append(messages, chunk)
							remaining := strings.TrimSpace(header + "\n" + string(runes[splitAt:totalLen]))
							runes = []rune(remaining)
							totalLen = len(runes)
							start = 0
							continue
						}
					}
				}
			}
		}

		if msgEnd <= start {
			msgEnd = start + effectiveLimit
		}

		messages = append(messages, string(runes[start:msgEnd]))
		// 推进 start，跳过下一分块开头处的空白字符
		start = msgEnd
		for start < totalLen && (runes[start] == ' ' || runes[start] == '\t' || runes[start] == '\n' || runes[start] == '\r') {
			start++
		}
	}

	return messages
}

// findLastUnclosedCodeBlockInRange 在 runes[start:end] 范围内找出最后一个
// 没有对应闭合 ``` 的开启 ```。返回绝对 rune 索引，未找到则返回 -1。
func findLastUnclosedCodeBlockInRange(runes []rune, start, end int) int {
	inCodeBlock := false
	lastOpenIdx := -1

	for i := start; i < end; i++ {
		if i+2 < end && runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			if !inCodeBlock {
				lastOpenIdx = i
			}
			inCodeBlock = !inCodeBlock
			i += 2
		}
	}

	if inCodeBlock {
		return lastOpenIdx
	}
	return -1
}

// findNextClosingCodeBlockInRange 从 startIdx 开始，在 runes[startIdx:end] 范围内
// 找出下一个闭合的 ```。返回闭合 ``` 之后的绝对索引，未找到则返回 -1。
func findNextClosingCodeBlockInRange(runes []rune, startIdx, end int) int {
	for i := startIdx; i < end; i++ {
		if i+2 < end && runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			return i + 3
		}
	}
	return -1
}

// findNewlineFrom 从给定索引开始查找第一个换行符。
// 返回绝对索引，未找到则返回 -1。
func findNewlineFrom(runes []rune, from int) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == '\n' {
			return i
		}
	}
	return -1
}

// findLastNewlineInRange 在 runes[start:end] 范围末尾的 searchWindow 个 rune 中
// 查找最后一个换行符。返回绝对索引，未找到则返回 start-1。
func findLastNewlineInRange(runes []rune, start, end, searchWindow int) int {
	searchStart := max(end-searchWindow, start)
	for i := end - 1; i >= searchStart; i-- {
		if runes[i] == '\n' {
			return i
		}
	}
	return start - 1
}

// findLastSpaceInRange 在 runes[start:end] 范围末尾的 searchWindow 个 rune 中
// 查找最后一个空格/制表符。返回绝对索引，未找到则返回 start-1。
func findLastSpaceInRange(runes []rune, start, end, searchWindow int) int {
	searchStart := max(end-searchWindow, start)
	for i := end - 1; i >= searchStart; i-- {
		if runes[i] == ' ' || runes[i] == '\t' {
			return i
		}
	}
	return start - 1
}
