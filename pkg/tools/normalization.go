package tools

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/rlds/chagent/pkg/media"
)

const (
	largeBase64OmittedMessage = "[工具返回了看起来像大段 base64 的内容，已从模型上下文省略。]"
	inlineMediaOmittedMessage = "[工具返回了内联媒体内容，已从模型上下文省略。]"
	inlineMediaStoredMessage  = "[工具返回了内联媒体内容（%s），已从模型上下文省略并登记为媒体附件。]"
)

var (
	inlineMarkdownDataURLRe = regexp.MustCompile(`!\[[^\]]*\]\((data:[^)]+)\)`)
	inlineRawDataURLRe      = regexp.MustCompile(`data:[^;\s]+;base64,[A-Za-z0-9+/=\r\n]+`)
)

func normalizeToolResult(
	result *ToolResult,
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
) *ToolResult {
	if result == nil {
		return nil
	}

	notes := make([]string, 0, 2)
	seen := make(map[string]struct{})

	if store != nil && channel != "" && chatID != "" {
		var refs []string
		var extractedNotes []string

		result.ForLLM, refs, extractedNotes = extractInlineMediaRefs(
			result.ForLLM,
			toolName,
			store,
			channel,
			chatID,
			seen,
		)
		result.Media = append(result.Media, refs...)
		notes = append(notes, extractedNotes...)

		result.ForUser, refs, extractedNotes = extractInlineMediaRefs(
			result.ForUser,
			toolName,
			store,
			channel,
			chatID,
			seen,
		)
		result.Media = append(result.Media, refs...)
		notes = append(notes, extractedNotes...)
	}

	if result.IsError {
		// 即使出错的参数让文本看起来像 base64，错误诊断信息也必须
		// 送达模型：否则模型只会看到一条笼统的“大段载荷已省略”提示，
		// 无法自行纠正（健壮性 B4）。错误内容在合理的诊断上限内保持完整。
		result.ForLLM = sanitizeErrorToolLLMContent(result.ForLLM)
	} else {
		result.ForLLM = sanitizeToolLLMContent(result.ForLLM)
	}

	if len(result.Media) > 0 && len(notes) > 0 {
		if strings.TrimSpace(result.ForLLM) == "" {
			result.ForLLM = strings.Join(notes, "\n")
		} else {
			result.ForLLM = strings.TrimSpace(result.ForLLM) + "\n" + strings.Join(notes, "\n")
		}
	}
	if len(result.Media) > 0 && strings.TrimSpace(result.ForLLM) == "" {
		result.ForLLM = "[工具返回了媒体内容，已从模型上下文省略并登记为媒体附件。]"
	}

	return result
}

func sanitizeToolLLMContent(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	if inlineMarkdownDataURLRe.MatchString(trimmed) || inlineRawDataURLRe.MatchString(trimmed) {
		cleaned := inlineMarkdownDataURLRe.ReplaceAllString(trimmed, "")
		cleaned = inlineRawDataURLRe.ReplaceAllString(cleaned, "")
		cleaned = strings.TrimSpace(cleaned)
		if cleaned == "" {
			return inlineMediaOmittedMessage
		}
		return cleaned + "\n" + inlineMediaOmittedMessage
	}
	if looksLikeLargeBase64Payload(trimmed) {
		return largeBase64OmittedMessage
	}
	return text
}

func looksLikeLargeBase64Payload(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 1024 {
		return false
	}

	nonSpace := 0
	base64Like := 0
	spaceCount := 0

	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			spaceCount++
			continue
		}
		nonSpace++
		if (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '+' || r == '/' || r == '=' {
			base64Like++
		}
	}

	if nonSpace == 0 {
		return false
	}

	ratio := float64(base64Like) / float64(nonSpace)
	return ratio >= 0.97 && spaceCount <= len(trimmed)/128
}

func extractInlineMediaRefs(
	text string,
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
	seen map[string]struct{},
) (cleaned string, refs []string, notes []string) {
	cleaned = text

	matches := inlineMarkdownDataURLRe.FindAllStringSubmatch(cleaned, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		dataURL := match[1]
		ref, note := storeInlineDataURL(toolName, store, channel, chatID, dataURL, seen)
		if ref != "" {
			refs = append(refs, ref)
		}
		if note != "" {
			notes = append(notes, note)
		}
		cleaned = strings.ReplaceAll(cleaned, match[0], "")
	}

	rawMatches := inlineRawDataURLRe.FindAllString(cleaned, -1)
	for _, dataURL := range rawMatches {
		ref, note := storeInlineDataURL(toolName, store, channel, chatID, dataURL, seen)
		if ref != "" {
			refs = append(refs, ref)
		}
		if note != "" {
			notes = append(notes, note)
		}
		cleaned = strings.ReplaceAll(cleaned, dataURL, "")
	}

	return strings.TrimSpace(cleaned), refs, notes
}

func storeInlineDataURL(
	toolName string,
	store media.MediaStore,
	channel string,
	chatID string,
	dataURL string,
	seen map[string]struct{},
) (ref string, note string) {
	dataURL = strings.TrimSpace(dataURL)
	if _, ok := seen[dataURL]; ok {
		return "", ""
	}
	seen[dataURL] = struct{}{}

	if !strings.HasPrefix(strings.ToLower(dataURL), "data:") {
		return "", ""
	}

	comma := strings.IndexByte(dataURL, ',')
	if comma <= 5 {
		return "", "[工具返回的内联媒体内容无法解析。]"
	}

	metaPart := dataURL[:comma]
	payload := dataURL[comma+1:]
	if !strings.Contains(strings.ToLower(metaPart), ";base64") {
		return "", "[工具返回的内联媒体内容不是 base64 编码。]"
	}

	mimeType := strings.TrimSpace(strings.TrimPrefix(metaPart, "data:"))
	if semi := strings.IndexByte(mimeType, ';'); semi >= 0 {
		mimeType = mimeType[:semi]
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	payload = strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(payload)
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法解码。]", mimeType)
	}

	dir := media.TempDir()
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法存储。]", mimeType)
	}

	ext := extensionForMIMEType(mimeType)
	tmpFile, err := os.CreateTemp(dir, "tool-inline-*"+ext)
	if err != nil {
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法存储。]", mimeType)
	}
	tmpPath := tmpFile.Name()
	if _, err = tmpFile.Write(decoded); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法存储。]", mimeType)
	}
	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法存储。]", mimeType)
	}

	filename := sanitizeIdentifierComponent(toolName) + ext
	scope := fmt.Sprintf(
		"tool:inline:%s:%s:%s:%d",
		sanitizeIdentifierComponent(toolName),
		channel,
		chatID,
		time.Now().UnixNano(),
	)

	ref, err = store.Store(tmpPath, media.MediaMeta{
		Filename:    filename,
		ContentType: mimeType,
		Source:      fmt.Sprintf("tool:inline:%s", sanitizeIdentifierComponent(toolName)),
	}, scope)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[工具返回的内联媒体内容（%s）无法登记。]", mimeType)
	}

	return ref, fmt.Sprintf(inlineMediaStoredMessage, mimeType)
}

func extensionForMIMEType(mimeType string) string {
	if mimeType == "" {
		return ".bin"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}

	switch strings.ToLower(mimeType) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	default:
		return filepath.Ext(mimeType)
	}
}

// maxErrorLLMBytes 限定模型可见的错误诊断信息大小。错误内容绝不会
// 经过大段 base64 启发式判断：参数即使写错且极为巨大，也必须
// 产生可读的校验错误，以支持模型自我纠正。
const (
	maxErrorLLMBytes = 16 * 1024
	// errorHeadBytes/errorTailBytes 保留长错误的上下文（开头部分）
	// 与终止原因（例如 "file name too long"）。
	errorHeadBytes = 8 * 1024
	errorTailBytes = 4 * 1024
)

func sanitizeErrorToolLLMContent(text string) string {
	if len(text) <= maxErrorLLMBytes {
		return text
	}
	headCut := errorHeadBytes
	for headCut > 0 && (text[headCut]&0xC0) == 0x80 {
		headCut--
	}
	tailStart := len(text) - errorTailBytes
	for tailStart < len(text) && (text[tailStart]&0xC0) == 0x80 {
		tailStart++
	}
	omitted := len(text) - headCut - (len(text) - tailStart)
	return text[:headCut] + fmt.Sprintf(
		"\n...[error truncated: %d bytes omitted]...\n", omitted,
	) + text[tailStart:]
}
