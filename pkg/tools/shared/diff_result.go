package toolshared

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

const (
	noContentChangeDiffMessage = "（无内容变更）"
	noNewlineAtEOFMarker       = `\ No newline at end of file`
	diffPreviewSkippedMessage  = "[diff 预览已跳过：文件过大，无法内联预览]"
	diffPreviewTruncatedNote   = "[diff 预览已截断；完整修改后内容请调用 read_file 查看]"
	maxDiffInputBytes          = 64 * 1024
	maxDiffInputLines          = 2000
	maxUserDiffPreviewBytes    = 16 * 1024
)

// DiffResult 创建一个用户可见的工具结果，其中包含针对成功文件编辑的
// 统一 diff。该 diff 同时提供给 LLM 和用户，以便后续的助手回复
// 能够基于最终的变更集进行推理，包括文件末尾换行（EOF newline）的变化。
func DiffResult(path string, before, after []byte) *ToolResult {
	summary := fmt.Sprintf("文件已编辑：%s", path)
	if exceedsDiffPreviewLimits(before, after) {
		return SilentResult(summary + "\n" + diffPreviewSkippedMessage)
	}

	diff, err := buildUnifiedDiff(path, before, after)
	if err != nil {
		return UserResult(fmt.Sprintf("%s\n[diff 不可用：%v]", summary, err))
	}

	userDiff, truncated := truncateDiffPreview(diff, maxUserDiffPreviewBytes)
	userContent := fmt.Sprintf("%s\n```diff\n%s\n```", summary, userDiff)
	if truncated {
		userContent += "\n" + diffPreviewTruncatedNote
	}

	llmContent := summary
	if diff == noContentChangeDiffMessage {
		llmContent = summary + "\n" + noContentChangeDiffMessage
	} else if truncated {
		llmContent = summary + "\n" + diffPreviewTruncatedNote
	}

	return &ToolResult{
		ForLLM:  llmContent,
		ForUser: userContent,
		Silent:  false,
		IsError: false,
		Async:   false,
	}
}

func buildUnifiedDiff(path string, before, after []byte) (string, error) {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        splitDiffLinesPreservingEOF(before),
		B:        splitDiffLinesPreservingEOF(after),
		FromFile: "a/" + diffDisplayPath(path),
		ToFile:   "b/" + diffDisplayPath(path),
		Context:  3,
	})
	if err != nil {
		return "", err
	}

	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return noContentChangeDiffMessage, nil
	}

	return diff, nil
}

func splitDiffLinesPreservingEOF(content []byte) []string {
	if len(content) == 0 {
		return nil
	}

	lines := make([]string, 0, bytes.Count(content, []byte{'\n'})+1)
	lineStart := 0
	for i, b := range content {
		if b != '\n' {
			continue
		}
		lines = append(lines, string(content[lineStart:i+1]))
		lineStart = i + 1
	}
	if lineStart < len(content) {
		lines = append(lines, string(content[lineStart:]))
	}

	if lacksTrailingNewline(content) {
		lines[len(lines)-1] += "\n"
		lines = append(lines, noNewlineAtEOFMarker+"\n")
	}

	return lines
}

func lacksTrailingNewline(content []byte) bool {
	return len(content) > 0 && !bytes.HasSuffix(content, []byte("\n"))
}

func exceedsDiffPreviewLimits(before, after []byte) bool {
	return len(before) > maxDiffInputBytes ||
		len(after) > maxDiffInputBytes ||
		countDiffLines(before) > maxDiffInputLines ||
		countDiffLines(after) > maxDiffInputLines
}

func countDiffLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}

	lines := bytes.Count(content, []byte{'\n'})
	if !bytes.HasSuffix(content, []byte("\n")) {
		lines++
	}
	return lines
}

func truncateDiffPreview(diff string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(diff) <= maxBytes {
		return diff, false
	}

	truncated := diff[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	lastNewline := strings.LastIndexByte(truncated, '\n')
	if lastNewline > 0 {
		truncated = truncated[:lastNewline]
	}

	truncated = strings.TrimRight(truncated, "\n")
	if truncated == "" {
		truncated = diff[:maxBytes]
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		truncated = strings.TrimRight(truncated, "\n")
	}

	return truncated, true
}

func diffDisplayPath(path string) string {
	displayPath := strings.TrimLeft(filepath.ToSlash(path), "/")
	if displayPath == "" {
		return "file"
	}
	return displayPath
}
