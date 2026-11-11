package integrationtools

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/h2non/filetype"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
	fstools "github.com/rlds/chagent/pkg/tools/fs"
)

type SendCallbackWithContext func(
	ctx context.Context,
	channel, chatID, content, replyToMessageID string,
	mediaParts []bus.MediaPart,
) error

type messageMediaArg struct {
	Path     string
	Type     string
	Filename string
}

// sentTarget 记录 message 工具发送到的 channel 与 chatID。
type sentTarget struct {
	Channel string
	ChatID  string
}

type MessageTool struct {
	sendCallback      SendCallbackWithContext
	workspace         string
	restrict          bool
	maxFileSize       int
	mediaStore        media.MediaStore
	allowPaths        []*regexp.Regexp
	localMediaEnabled bool
	mu                sync.Mutex
	sentTargets       map[string][]sentTarget
}

func NewMessageTool() *MessageTool {
	return &MessageTool{
		sentTargets: make(map[string][]sentTarget),
	}
}

func (t *MessageTool) Name() string {
	return "message"
}

func (t *MessageTool) Description() string {
	if !t.localMediaEnabled {
		return "在聊天通道上向用户发送文本消息。"
	}
	return "在聊天通道上向用户发送消息。支持纯文本、纯媒体或带媒体附件的文本。"
}

func (t *MessageTool) Parameters() map[string]any {
	properties := map[string]any{
		"content": map[string]any{
			"type":        "string",
			"description": "可选的消息文本。存在媒体时，该文本作为媒体消息的说明/正文。",
		},
		"channel": map[string]any{
			"type":        "string",
			"description": "可选：目标通道（telegram、whatsapp 等）",
		},
		"chat_id": map[string]any{
			"type":        "string",
			"description": "可选：目标聊天/用户 ID",
		},
		"reply_to_message_id": map[string]any{
			"type":        "string",
			"description": "可选：支持线程回复的通道的回复目标消息 ID",
		},
	}
	params := map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"content"},
	}
	if t.localMediaEnabled {
		properties["media"] = map[string]any{
			"type":        "array",
			"description": "可选的本地媒体附件，随消息一起发送。需要启用 tools.message.media_enabled。",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "本地文件路径。相对路径从工作区解析。",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "可选媒体类型提示：image、audio、video 或 file。",
					},
					"filename": map[string]any{
						"type":        "string",
						"description": "可选的展示文件名。默认使用 path 的文件名。",
					},
				},
				"required": []string{"path"},
			},
		}
		delete(params, "required")
		params["anyOf"] = []map[string]any{
			{"required": []string{"content"}},
			{"required": []string{"media"}},
		}
	}
	return params
}

func (t *MessageTool) ConfigureLocalMedia(
	workspace string,
	restrict bool,
	maxFileSize int,
	allowPaths []*regexp.Regexp,
) {
	t.workspace = workspace
	t.restrict = restrict
	if maxFileSize <= 0 {
		maxFileSize = config.DefaultMaxMediaSize
	}
	t.maxFileSize = maxFileSize
	t.allowPaths = allowPaths
	t.localMediaEnabled = true
}

func (t *MessageTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

// ResetSentInRound 重置给定会话键在本轮的发送记录。
// 由 agent 循环在处理每条入站消息的一轮开始时调用。
func (t *MessageTool) ResetSentInRound(sessionKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 彻底删除该键，以免会话数量众多、彼此不同时 map 无限增长。
	// 只截断切片会让键继续存在。
	delete(t.sentTargets, sessionKey)
}

// HasSentInRound 返回 message 工具在本轮是否发送过消息。
func (t *MessageTool) HasSentInRound(sessionKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sentTargets[sessionKey]) > 0
}

// HasSentTo 返回 message 工具在本轮是否向指定的 channel+chatID 发送过消息。
// 供 PublishResponseIfNeeded 使用：当 message 工具只发送到了另一个会话时，
// 避免因此抑制最终回复。
func (t *MessageTool) HasSentTo(sessionKey, channel, chatID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, st := range t.sentTargets[sessionKey] {
		if st.Channel == channel && st.ChatID == chatID {
			return true
		}
	}
	return false
}

func (t *MessageTool) SetSendCallback(callback SendCallbackWithContext) {
	t.sendCallback = callback
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	content, _ := args["content"].(string)
	content = strings.TrimSpace(content)
	mediaArgs, err := parseMessageMediaArgs(args["media"])
	if err != nil {
		return &ToolResult{ForLLM: err.Error(), IsError: true}
	}
	if len(mediaArgs) > 0 && !t.localMediaEnabled {
		return &ToolResult{
			ForLLM:  "消息媒体附件已禁用；请启用 tools.message.media_enabled 后再通过 message 工具发送本地媒体",
			IsError: true,
		}
	}
	if content == "" && len(mediaArgs) == 0 {
		return &ToolResult{ForLLM: "必须提供 content 或 media", IsError: true}
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	replyToMessageID, _ := args["reply_to_message_id"].(string)

	if channel == "" {
		channel = ToolChannel(ctx)
	}
	if chatID == "" {
		chatID = ToolChatID(ctx)
	}

	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "未指定目标 channel/chat", IsError: true}
	}

	if t.sendCallback == nil {
		return &ToolResult{ForLLM: "消息发送未配置", IsError: true}
	}

	parts, err := t.buildMediaParts(channel, chatID, content, mediaArgs)
	if err != nil {
		return &ToolResult{ForLLM: err.Error(), IsError: true, Err: err}
	}

	if err := t.sendCallback(ctx, channel, chatID, content, replyToMessageID, parts); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("发送消息失败：%v", err),
			IsError: true,
			Err:     err,
		}
	}

	sessionKey := ToolSessionKey(ctx)
	t.mu.Lock()
	t.sentTargets[sessionKey] = append(t.sentTargets[sessionKey], sentTarget{Channel: channel, ChatID: chatID})
	t.mu.Unlock()

	status := fmt.Sprintf("消息已发送至 %s:%s", channel, chatID)
	if len(parts) > 0 {
		status = fmt.Sprintf("带 %d 个媒体附件的消息已发送至 %s:%s", len(parts), channel, chatID)
	}

	return &ToolResult{
		ForLLM: status,
		Silent: true,
	}
}

func parseMessageMediaArgs(raw any) ([]messageMediaArg, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("media 必须是数组")
	}
	result := make([]messageMediaArg, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("media[%d] 必须是对象", i)
		}
		path, _ := obj["path"].(string)
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("media[%d].path 为必填项", i)
		}
		typ, _ := obj["type"].(string)
		filename, _ := obj["filename"].(string)
		result = append(result, messageMediaArg{
			Path:     path,
			Type:     strings.TrimSpace(typ),
			Filename: strings.TrimSpace(filename),
		})
	}
	return result, nil
}

func (t *MessageTool) buildMediaParts(
	channel, chatID, content string,
	mediaArgs []messageMediaArg,
) ([]bus.MediaPart, error) {
	if len(mediaArgs) == 0 {
		return nil, nil
	}
	if !t.localMediaEnabled {
		return nil, fmt.Errorf("消息媒体附件已禁用")
	}
	if t.mediaStore == nil {
		return nil, fmt.Errorf("媒体存储未配置")
	}
	if strings.TrimSpace(t.workspace) == "" {
		return nil, fmt.Errorf("消息媒体投递未配置")
	}

	scope := fmt.Sprintf("tool:message:%s:%s", channel, chatID)
	parts := make([]bus.MediaPart, 0, len(mediaArgs))
	for i, item := range mediaArgs {
		resolved, err := fstools.ValidatePathWithAllowPaths(item.Path, t.workspace, t.restrict, t.allowPaths)
		if err != nil {
			return nil, fmt.Errorf("media[%d].path 无效：%w", i, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("media[%d] 文件不存在：%w", i, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("media[%d] 路径是目录，需要文件", i)
		}
		if t.maxFileSize > 0 && info.Size() > int64(t.maxFileSize) {
			return nil, fmt.Errorf("media[%d] 文件过大：%d 字节（上限 %d 字节）", i, info.Size(), t.maxFileSize)
		}

		filename := item.Filename
		if filename == "" {
			filename = filepath.Base(resolved)
		}
		contentType := detectMessageMediaType(resolved)
		partType := normalizeMessageMediaType(item.Type, filename, contentType)
		ref, err := t.mediaStore.Store(resolved, media.MediaMeta{
			Filename:      filename,
			ContentType:   contentType,
			Source:        "tool:message",
			CleanupPolicy: media.CleanupPolicyForgetOnly,
		}, scope)
		if err != nil {
			return nil, fmt.Errorf("注册 media[%d] 失败：%w", i, err)
		}

		part := bus.MediaPart{
			Type:        partType,
			Ref:         ref,
			Filename:    filename,
			ContentType: contentType,
		}
		if i == 0 && content != "" {
			part.Caption = content
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func detectMessageMediaType(path string) string {
	kind, err := filetype.MatchFile(path)
	if err == nil && kind != filetype.Unknown {
		return kind.MIME.Value
	}
	if ext := filepath.Ext(path); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return "application/octet-stream"
}

func normalizeMessageMediaType(typeHint, filename, contentType string) string {
	switch strings.ToLower(strings.TrimSpace(typeHint)) {
	case "image", "audio", "video", "file":
		return strings.ToLower(strings.TrimSpace(typeHint))
	}

	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	}

	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return "image"
	case ".mp3", ".wav", ".ogg", ".oga", ".m4a", ".flac":
		return "audio"
	case ".mp4", ".mov", ".mkv", ".webm", ".avi":
		return "video"
	default:
		return "file"
	}
}
