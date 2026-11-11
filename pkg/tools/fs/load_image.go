package fstools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
)

// LoadImageTool 将本地图片文件加载到 MediaStore 并返回一个 media:// 引用。
// 随后 agent 循环中的 resolveMediaRefs 会对其做 base64 编码，并作为 image_url
// 部分附加到下一次 LLM 请求中，从而让本地文件也具备视觉能力 —— 这与用户通过
// 聊天渠道发送图片时使用的是同一条处理管线。
//
// 它与 SendFileTool 有刻意区别：
//   - SendFileTool  → MediaResult + WithResponseHandled() → 将文件发送给用户并结束本轮
//   - LoadImageTool → 在 ForLLM 中带 media:// 的普通 ToolResult  → LLM 在下一轮看到图片
type LoadImageTool struct {
	workspace   string
	restrict    bool
	maxFileSize int
	mediaStore  media.MediaStore
	allowPaths  []*regexp.Regexp

	defaultChannel string
	defaultChatID  string
}

func NewLoadImageTool(
	workspace string,
	restrict bool,
	maxFileSize int,
	store media.MediaStore,
	allowPaths ...[]*regexp.Regexp,
) *LoadImageTool {
	if maxFileSize <= 0 {
		maxFileSize = config.DefaultMaxMediaSize
	}
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &LoadImageTool{
		workspace:   workspace,
		restrict:    restrict,
		maxFileSize: maxFileSize,
		mediaStore:  store,
		allowPaths:  patterns,
	}
}

func (t *LoadImageTool) Name() string { return "load_image" }

func (t *LoadImageTool) Description() string {
	return "加载本地图片文件，以便通过视觉能力分析其内容。" +
		"支持格式：JPEG、PNG、GIF、WebP、BMP。" +
		"调用后请在下一轮回复中描述或分析该图片。"
}

func (t *LoadImageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "本地图片文件路径。相对路径从工作区解析。",
			},
		},
		"required": []string{"path"},
	}
}

func (t *LoadImageTool) SetContext(channel, chatID string) {
	t.defaultChannel = channel
	t.defaultChatID = chatID
}

func (t *LoadImageTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

func (t *LoadImageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
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
		return ErrorResult("路径是目录，应为图片文件")
	}
	if info.Size() > int64(t.maxFileSize) {
		return ErrorResult(fmt.Sprintf(
			"文件过大：%d 字节（上限 %d 字节）", info.Size(), t.maxFileSize,
		))
	}

	// 检测 MIME 类型 —— 复用 send_file.go 中已有的辅助函数
	mediaType := detectMediaType(resolved)
	if !strings.HasPrefix(mediaType, "image/") {
		return ErrorResult(fmt.Sprintf(
			"文件看起来不是图片（检测到类型：%s）", mediaType,
		))
	}

	filename := filepath.Base(resolved)
	scope := fmt.Sprintf("tool:load_image:%s:%s", channel, chatID)

	ref, err := t.mediaStore.Store(resolved, media.MediaMeta{
		Filename:      filename,
		ContentType:   mediaType,
		Source:        "tool:load_image",
		CleanupPolicy: media.CleanupPolicyForgetOnly,
	}, scope)
	if err != nil {
		return ErrorResult(fmt.Sprintf("将图片登记到 media store 失败：%v", err))
	}

	// 构造工具结果文本。Media 中的 media:// 引用会被 agent_media.go 中的
	// resolveMediaRefs 拾取，并做 base64 编码后放入工具结果消息（role="tool"），
	// 这样 LLM 就能看到图片内容。
	msg := fmt.Sprintf("图片已加载：%s\n[image: photo]", filename)

	return &ToolResult{
		ForLLM:  msg,
		ForUser: fmt.Sprintf("已加载图片：%s", filename),
		// ForLLM 中的媒体引用会在下一次 LLM 调用前由 agent 循环中的
		// resolveMediaRefs 解析。此处不要使用 MediaResult —— 那会把文件发送到
		// 用户渠道，而不是交给 LLM。
		Media: []string{ref},
	}
}
