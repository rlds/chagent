// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/h2non/filetype"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers"
)

// genericPlaceholderRegex 用于匹配各类渠道发出的通用媒体占位符：
// [image]、[image: photo]、[image: filename.jpg] —— 但不匹配路径标签
// 形如 [image:/path/to/file]（路径标签冒号后没有空格）。
var (
	imagePlaceholderRegex = regexp.MustCompile(`\[image(:\s+[^\]]*)?\]`)
	audioPlaceholderRegex = regexp.MustCompile(`\[audio(:\s+[^\]]*)?\]`)
	videoPlaceholderRegex = regexp.MustCompile(`\[video(:\s+[^\]]*)?\]`)
	filePlaceholderRegex  = regexp.MustCompile(`\[file(:\s+[^\]]*)?\]`)
)

func normalizeCurrentTurnStart(messages []providers.Message, currentTurnStart int) int {
	if currentTurnStart < 0 {
		return 0
	}
	if currentTurnStart > len(messages) {
		return len(messages)
	}
	return currentTurnStart
}

func currentTurnMessages(messages []providers.Message, currentTurnStart int) []providers.Message {
	currentTurnStart = normalizeCurrentTurnStart(messages, currentTurnStart)
	return messages[currentTurnStart:]
}

// resolveMediaRefs 解析消息中的 media:// 引用。
// 对用户消息：图片只生成路径标签（[image:/path]），以便 LLM
// 自行决定是通过 load_image 查看它们，还是直接操作该文件。
// 对工具消息：图片会被 base64 编码，并作为一条合成的用户消息，
// 仅在连续的 tool 消息块结束后追加，这样就不会破坏
// “工具结果必须紧跟在 assistant 消息之后”这一 LLM API 强制约束。
// 只有当前轮次的工具消息才会触发这条合成的用户后续消息；
// 历史工具结果保持为普通的带路径标签历史。
// 非图片文件无论角色如何，始终生成路径标签。
// 返回新切片；原始消息不会被修改。
func resolveMediaRefs(
	messages []providers.Message,
	store media.MediaStore,
	maxSize int,
	currentTurnStart int,
) []providers.Message {
	if store == nil {
		return messages
	}
	currentTurnStart = normalizeCurrentTurnStart(messages, currentTurnStart)

	result := make([]providers.Message, 0, len(messages))
	var pendingToolImages []string

	for idx, m := range messages {
		// 离开 tool 消息块时，把累积的图片作为一条合成的用户消息
		// 刷出。
		if m.Role != "tool" && len(pendingToolImages) > 0 {
			result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
			pendingToolImages = nil
		}

		if len(m.Media) == 0 {
			result = append(result, m)
			if idx == len(messages)-1 && len(pendingToolImages) > 0 {
				result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
				pendingToolImages = nil
			}
			continue
		}

		msg := m
		resolved := make([]string, 0, len(m.Media))
		var pathTags []string

		for _, ref := range m.Media {
			if !strings.HasPrefix(ref, "media://") {
				resolved = append(resolved, ref)
				continue
			}

			localPath, meta, err := store.ResolveWithMeta(ref)
			if err != nil {
				logger.WarnCF("agent", "Failed to resolve media ref", map[string]any{
					"ref":   ref,
					"error": err.Error(),
				})
				continue
			}

			info, err := os.Stat(localPath)
			if err != nil {
				logger.WarnCF("agent", "Failed to stat media file", map[string]any{
					"path":  localPath,
					"error": err.Error(),
				})
				continue
			}

			mime := detectMIME(localPath, meta)
			pathTags = append(pathTags, buildPathTag(mime, localPath))

			if m.Role == "tool" && idx >= currentTurnStart && strings.HasPrefix(mime, "image/") {
				dataURL := encodeImageToDataURL(localPath, mime, info, maxSize)
				if dataURL != "" {
					pendingToolImages = append(pendingToolImages, dataURL)
				}
			}
		}

		msg.Media = resolved
		if len(pathTags) > 0 {
			msg.Content = injectPathTags(msg.Content, pathTags)
		}
		result = append(result, msg)

		// 如果这是最后一条消息且存在待刷出的图片，则刷出它们。
		if idx == len(messages)-1 && len(pendingToolImages) > 0 {
			result = append(result, toolImageFollowUpPromptMessage(pendingToolImages))
			pendingToolImages = nil
		}
	}

	return result
}

// encodeImageToDataURL 将图片文件 base64 编码为 data URL。
// 如果文件超过 maxSize 或编码失败，则返回空字符串。
func encodeImageToDataURL(localPath, mime string, info os.FileInfo, maxSize int) string {
	if info.Size() > int64(maxSize) {
		logger.WarnCF("agent", "Media file too large, skipping", map[string]any{
			"path":     localPath,
			"size":     info.Size(),
			"max_size": maxSize,
		})
		return ""
	}

	f, err := os.Open(localPath)
	if err != nil {
		logger.WarnCF("agent", "Failed to open media file", map[string]any{
			"path":  localPath,
			"error": err.Error(),
		})
		return ""
	}
	defer f.Close()

	prefix := "data:" + mime + ";base64,"
	encodedLen := base64.StdEncoding.EncodedLen(int(info.Size()))
	var buf bytes.Buffer
	buf.Grow(len(prefix) + encodedLen)
	buf.WriteString(prefix)

	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	_, copyErr := io.Copy(encoder, f)
	closeErr := encoder.Close()
	if copyErr != nil {
		logger.WarnCF("agent", "Failed to encode media file", map[string]any{
			"path":  localPath,
			"error": copyErr.Error(),
		})
		return ""
	}
	if closeErr != nil {
		logger.WarnCF("agent", "Failed to close base64 encoder", map[string]any{
			"path":  localPath,
			"error": closeErr.Error(),
		})
		return ""
	}

	return buf.String()
}

func buildArtifactTags(store media.MediaStore, refs []string) []string {
	if store == nil || len(refs) == 0 {
		return nil
	}

	tags := make([]string, 0, len(refs))
	for _, ref := range refs {
		localPath, meta, err := store.ResolveWithMeta(ref)
		if err != nil {
			continue
		}
		mime := detectMIME(localPath, meta)
		tags = append(tags, buildPathTag(mime, localPath))
	}

	return tags
}

func buildProviderAttachments(store media.MediaStore, refs []string) []providers.Attachment {
	if store == nil || len(refs) == 0 {
		return nil
	}

	attachments := make([]providers.Attachment, 0, len(refs))
	for _, ref := range refs {
		attachment := providers.Attachment{Ref: ref}
		if _, meta, err := store.ResolveWithMeta(ref); err == nil {
			attachment.Filename = meta.Filename
			attachment.ContentType = meta.ContentType
			attachment.Type = inferMediaType(meta.Filename, meta.ContentType)
		}
		attachments = append(attachments, attachment)
	}

	return attachments
}

// detectMIME 根据元数据或魔数检测来确定 MIME 类型。
// 检测失败时返回空字符串。
func detectMIME(localPath string, meta media.MediaMeta) string {
	if meta.ContentType != "" {
		return meta.ContentType
	}
	kind, err := filetype.MatchFile(localPath)
	if err != nil || kind == filetype.Unknown {
		return ""
	}
	return kind.MIME.Value
}

// buildPathTag 创建一个暴露本地文件路径的结构化标签。
// 标签类型由 MIME 推导：[image:/path]、[audio:/path]、[video:/path] 或 [file:/path]。
func buildPathTag(mime, localPath string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "[image:" + localPath + "]"
	case strings.HasPrefix(mime, "audio/"):
		return "[audio:" + localPath + "]"
	case strings.HasPrefix(mime, "video/"):
		return "[video:" + localPath + "]"
	default:
		return "[file:" + localPath + "]"
	}
}

// injectPathTags 将 content 中的通用媒体标签替换为带路径的版本，
// 如果没有找到匹配的通用标签，则追加。渠道会发出几种不同的
// 占位符格式 —— [image]、[image: photo]、[image: filename.jpg] —— 因此我们
// 用正则匹配所有这些格式，同时保持路径标签（[image:/path]）不变。
//
// 当 content 是结构化数据时（例如来自飞书互动卡片或
// 富文本消息的 JSON），只通过占位符替换来注入标签 —— 绝不
// 追加 —— 以免破坏载荷。
func injectPathTags(content string, tags []string) string {
	isStructured := looksLikeJSON(content)
	for _, tag := range tags {
		var pattern *regexp.Regexp
		switch {
		case strings.HasPrefix(tag, "[image:"):
			pattern = imagePlaceholderRegex
		case strings.HasPrefix(tag, "[audio:"):
			pattern = audioPlaceholderRegex
		case strings.HasPrefix(tag, "[video:"):
			pattern = videoPlaceholderRegex
		case strings.HasPrefix(tag, "[file:"):
			pattern = filePlaceholderRegex
		}

		if pattern != nil {
			if loc := pattern.FindStringIndex(content); loc != nil {
				content = content[:loc[0]] + tag + content[loc[1]:]
				continue
			}
		}

		if isStructured {
			content = tag + "\n" + content
			continue
		}

		if content == "" {
			content = tag
		} else {
			content += " " + tag
		}
	}
	return content
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) > 1 && s[0] == '{'
}
