package fstools

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/h2non/filetype"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
)

// SendFileTool 允许 LLM 通过 MediaStore 管线，把本地文件（图片、文档等）
// 发送给当前聊天通道的用户。
type SendFileTool struct {
	workspace   string
	restrict    bool
	maxFileSize int
	mediaStore  media.MediaStore
	allowPaths  []*regexp.Regexp

	defaultChannel string
	defaultChatID  string
}

func NewSendFileTool(
	workspace string,
	restrict bool,
	maxFileSize int,
	store media.MediaStore,
	allowPaths ...[]*regexp.Regexp,
) *SendFileTool {
	if maxFileSize <= 0 {
		maxFileSize = config.DefaultMaxMediaSize
	}
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &SendFileTool{
		workspace:   workspace,
		restrict:    restrict,
		maxFileSize: maxFileSize,
		mediaStore:  store,
		allowPaths:  patterns,
	}
}

func (t *SendFileTool) Name() string { return "send_file" }
func (t *SendFileTool) Description() string {
	return "把本地文件（图片、文档等）发送给当前聊天通道的用户。"
}

func (t *SendFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "本地文件路径。相对路径从工作区解析。",
			},
			"filename": map[string]any{
				"type":        "string",
				"description": "可选显示文件名。默认使用路径的文件名。",
			},
		},
		"required": []string{"path"},
	}
}

func (t *SendFileTool) SetContext(channel, chatID string) {
	t.defaultChannel = channel
	t.defaultChatID = chatID
}

func (t *SendFileTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

func (t *SendFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return ErrorResult("path 参数必填")
	}

	// 优先使用上下文注入的 channel/chatID（由 ExecuteWithContext 设置），否则回退到 SetContext 的值。
	channel := ToolChannel(ctx)
	if channel == "" {
		channel = t.defaultChannel
	}
	chatID := ToolChatID(ctx)
	if chatID == "" {
		chatID = t.defaultChatID
	}
	if channel == "" || chatID == "" {
		return ErrorResult("没有可用的目标 channel/chat")
	}

	if t.mediaStore == nil {
		return ErrorResult("未配置 media store")
	}

	resolved, err := validatePathWithAllowPaths(path, t.workspace, t.restrict, t.allowPaths)
	if err != nil {
		return ErrorResult(fmt.Sprintf("无效路径：%v", err))
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return ErrorResult(fmt.Sprintf("文件不存在：%v", err))
	}
	if info.IsDir() {
		return ErrorResult("路径是目录，应为文件")
	}
	if info.Size() > int64(t.maxFileSize) {
		return ErrorResult(fmt.Sprintf(
			"文件过大：%d 字节（上限 %d 字节）",
			info.Size(), t.maxFileSize,
		))
	}

	filename, _ := args["filename"].(string)
	if filename == "" {
		filename = filepath.Base(resolved)
	}

	mediaType := detectMediaType(resolved)
	scope := fmt.Sprintf("tool:send_file:%s:%s", channel, chatID)

	ref, err := t.mediaStore.Store(resolved, media.MediaMeta{
		Filename:      filename,
		ContentType:   mediaType,
		Source:        "tool:send_file",
		CleanupPolicy: media.CleanupPolicyForgetOnly,
	}, scope)
	if err != nil {
		return ErrorResult(fmt.Sprintf("登记媒体失败：%v", err))
	}

	return MediaResult(fmt.Sprintf("文件 %q 已发送给用户", filename), []string{ref}).WithResponseHandled()
}

// detectMediaType 判定文件的 MIME 类型。
// 先使用魔数检测（h2non/filetype），再回退到通过 mime.TypeByExtension
// 按扩展名查找。
func detectMediaType(path string) string {
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
