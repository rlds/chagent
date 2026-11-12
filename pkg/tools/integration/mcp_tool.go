package integrationtools

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// MCPManager 定义 MCP 管理器操作的接口。
// 这样可以更方便地使用 mock 实现进行测试。
type MCPManager interface {
	CallTool(
		ctx context.Context,
		serverName, toolName string,
		arguments map[string]any,
	) (*mcp.CallToolResult, error)
}

// MCPTool 包装 MCP 工具以实现 Tool 接口。
type MCPTool struct {
	manager            MCPManager
	serverName         string
	tool               *mcp.Tool
	mediaStore         media.MediaStore
	workspace          string
	maxInlineTextRunes int
	runtimeEvents      runtimeevents.Bus
}

// MCPToolCallPayload 描述 MCP 工具执行的运行时事件。
type MCPToolCallPayload struct {
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Error      string `json:"error,omitempty"`
}

// NewMCPTool 创建新的 MCP 工具包装器。
func NewMCPTool(manager MCPManager, serverName string, tool *mcp.Tool) *MCPTool {
	return &MCPTool{
		manager:            manager,
		serverName:         serverName,
		tool:               tool,
		maxInlineTextRunes: maxMCPInlineTextRunes,
	}
}

func (t *MCPTool) SetMediaStore(store media.MediaStore) {
	t.mediaStore = store
}

func (t *MCPTool) SetWorkspace(workspace string) {
	t.workspace = strings.TrimSpace(workspace)
}

func (t *MCPTool) SetMaxInlineTextRunes(limit int) {
	if limit > 0 {
		t.maxInlineTextRunes = limit
	}
}

// SetEventPublisher 注入用于 MCP 工具观测的运行时事件总线。
func (t *MCPTool) SetEventPublisher(eventBus runtimeevents.Bus) {
	t.runtimeEvents = eventBus
}

const maxMCPInlineTextRunes = 16 * 1024

// sanitizeIdentifierComponent 规范化字符串，使其可安全用作下游
// 提供方的工具/函数标识符的一部分。
// 它会：
//   - 将字符串转为小写
//   - 将不在 [a-z0-9_-] 中的字符替换为 '_'
//   - 将多个连续的 '_' 合并为单个 '_'
//   - 去除首尾的 '_'
//   - 结果为空时回退为 "unnamed"
//   - 将过长的组件截断到合理长度
func sanitizeIdentifierComponent(s string) string {
	const maxLen = 64

	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))

	prevUnderscore := false
	for _, r := range s {
		isAllowed := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'

		if !isAllowed {
			// 将任何不允许的字符规范化为 '_'
			if !prevUnderscore {
				b.WriteRune('_')
				prevUnderscore = true
			}
			continue
		}

		if r == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}

		b.WriteRune(r)
	}

	result := strings.Trim(b.String(), "_")
	if result == "" {
		result = "unnamed"
	}

	if len(result) > maxLen {
		result = result[:maxLen]
	}

	return result
}

// MCPServerName 返回该 MCP 工具所属的服务器名（原始大小写，未规范化）。
//
// 它供执行上报（execution.report）解析 mcp_server 使用：扁平化后的工具名
// `mcp_<server>_<tool>` 在服务器名或工具名含下划线时无法可靠反解，
// 从包装器直接读取来源名才是权威答案。
func (t *MCPTool) MCPServerName() string {
	if t == nil {
		return ""
	}
	return t.serverName
}

// MCPToolName 返回 MCP 服务器侧的原始工具名（未规范化）。
func (t *MCPTool) MCPToolName() string {
	if t == nil || t.tool == nil {
		return ""
	}
	return t.tool.Name
}

// Name 返回工具名，并加上服务器名前缀。
// 总长度上限为 64 个字符（兼容 OpenAI 的 API 限制）。
// 当规范化有损或名称被截断时，会追加原始（未规范化）服务器名和工具名的
// 短哈希，确保仅在不允许字符上不同的两个名称在规范化后仍保持区别。
func (t *MCPTool) Name() string {
	// 加上服务器名前缀以避免冲突，并规范化各组件
	sanitizedServer := sanitizeIdentifierComponent(t.serverName)
	sanitizedTool := sanitizeIdentifierComponent(t.tool.Name)
	full := fmt.Sprintf("mcp_%s_%s", sanitizedServer, sanitizedTool)

	// 检查规范化是否无损（仅转为小写，没有替换字符或截断）
	lossless := strings.ToLower(t.serverName) == sanitizedServer &&
		strings.ToLower(t.tool.Name) == sanitizedTool

	const maxTotal = 64
	if lossless && len(full) <= maxTotal {
		return full
	}

	// 规范化有损或名称过长：追加原始名称（而非规范化后的名称）的哈希，
	// 这样不同的原始名称总能得到不同的哈希。
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.serverName + "\x00" + t.tool.Name))
	suffix := fmt.Sprintf("%08x", h.Sum32()) // 8 个字符

	base := full
	if len(base) > maxTotal-9 {
		base = strings.TrimRight(full[:maxTotal-9], "_")
	}
	return base + "_" + suffix
}

// Description 返回工具描述。
func (t *MCPTool) Description() string {
	desc := t.tool.Description
	if desc == "" {
		desc = fmt.Sprintf("来自 %s 服务器的 MCP 工具", t.serverName)
	}
	// 在描述中添加服务器信息
	return fmt.Sprintf("[MCP:%s] %s", t.serverName, desc)
}

func (t *MCPTool) PromptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  toolshared.ToolPromptLayerCapability,
		Slot:   toolshared.ToolPromptSlotMCP,
		Source: "mcp:" + sanitizeIdentifierComponent(t.serverName),
	}
}

// Parameters 返回工具参数 schema。
func (t *MCPTool) Parameters() map[string]any {
	// InputSchema 本身已经是 JSON Schema 对象
	schema := t.tool.InputSchema

	// 处理 nil schema
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	// 先尝试直接转换（快速路径）
	if schemaMap, ok := schema.(map[string]any); ok {
		return schemaMap
	}

	// 处理 json.RawMessage 和 []byte——直接反序列化
	var jsonData []byte
	if rawMsg, ok := schema.(json.RawMessage); ok {
		jsonData = rawMsg
	} else if bytes, ok := schema.([]byte); ok {
		jsonData = bytes
	}

	if jsonData != nil {
		var result map[string]any
		if err := json.Unmarshal(jsonData, &result); err == nil {
			return result
		}
		// 出错时回退
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	// 对于其他类型（结构体等），通过 JSON 序列化/反序列化转换
	var err error
	jsonData, err = json.Marshal(schema)
	if err != nil {
		// 序列化失败时回退为空 schema
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	var result map[string]any
	if err := json.Unmarshal(jsonData, &result); err != nil {
		// 反序列化失败时回退为空 schema
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	return result
}

// Execute 执行 MCP 工具。
func (t *MCPTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	startedAt := time.Now()
	t.publishRuntimeEvent(ctx, runtimeevents.KindMCPToolCallStart, startedAt, false, "")

	result, err := t.manager.CallTool(ctx, t.serverName, t.tool.Name, args)
	if err != nil {
		t.publishRuntimeEvent(ctx, runtimeevents.KindMCPToolCallEnd, startedAt, true, err.Error())
		return ErrorResult(fmt.Sprintf("MCP 工具执行失败：%v", err)).WithError(err)
	}

	if result == nil {
		nilErr := fmt.Errorf("MCP 工具返回空结果且未报错")
		t.publishRuntimeEvent(ctx, runtimeevents.KindMCPToolCallEnd, startedAt, true, nilErr.Error())
		return ErrorResult("MCP 工具执行失败：结果为空").WithError(nilErr)
	}

	// 处理服务端返回的错误结果
	if result.IsError {
		errMsg := extractContentText(result.Content)
		t.publishRuntimeEvent(ctx, runtimeevents.KindMCPToolCallEnd, startedAt, true, errMsg)
		return ErrorResult(fmt.Sprintf("MCP 工具返回错误：%s", errMsg)).
			WithError(fmt.Errorf("MCP 工具错误：%s", errMsg))
	}

	t.publishRuntimeEvent(ctx, runtimeevents.KindMCPToolCallEnd, startedAt, false, "")
	return t.normalizeResultContent(ctx, result.Content, result.StructuredContent)
}

func (t *MCPTool) publishRuntimeEvent(
	ctx context.Context,
	kind runtimeevents.Kind,
	startedAt time.Time,
	isError bool,
	errMsg string,
) {
	if t == nil || t.runtimeEvents == nil {
		return
	}

	scope := runtimeevents.Scope{
		AgentID:    toolshared.ToolAgentID(ctx),
		SessionKey: toolshared.ToolSessionKey(ctx),
		Channel:    toolshared.ToolChannel(ctx),
		ChatID:     toolshared.ToolChatID(ctx),
		MessageID:  toolshared.ToolMessageID(ctx),
	}
	payload := MCPToolCallPayload{
		Server:     t.serverName,
		Tool:       t.tool.Name,
		DurationMS: time.Since(startedAt).Milliseconds(),
		IsError:    isError,
		Error:      errMsg,
	}
	severity := runtimeevents.SeverityInfo
	if isError {
		severity = runtimeevents.SeverityError
	}

	t.runtimeEvents.PublishNonBlocking(runtimeevents.Event{
		Kind:     kind,
		Source:   runtimeevents.Source{Component: "mcp", Name: t.serverName},
		Scope:    scope,
		Severity: severity,
		Payload:  payload,
		Attrs:    mcpToolCallEventAttrs(payload),
	})
}

func mcpToolCallEventAttrs(payload MCPToolCallPayload) map[string]any {
	attrs := map[string]any{
		"server":      payload.Server,
		"tool":        payload.Tool,
		"duration_ms": payload.DurationMS,
	}
	if payload.IsError {
		attrs["is_error"] = payload.IsError
	}
	if payload.Error != "" {
		attrs["error"] = payload.Error
	}
	return attrs
}

// extractContentText 从 MCP 内容数组中提取文本。
func extractContentText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, sanitizeToolLLMContent(v.Text))
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[图片：%s]", normalizedMIMEType(v.MIMEType)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[音频：%s]", normalizedMIMEType(v.MIMEType)))
		case *mcp.ResourceLink:
			parts = append(parts, summarizeResourceLink(v))
		case *mcp.EmbeddedResource:
			parts = append(parts, summarizeEmbeddedResource(v))
		default:
			// 对于其他内容类型，使用字符串表示
			parts = append(parts, fmt.Sprintf("[内容：%T]", v))
		}
	}
	return sanitizeToolLLMContent(strings.Join(parts, "\n"))
}

func (t *MCPTool) normalizeResultContent(
	ctx context.Context,
	content []mcp.Content,
	structuredContent any,
) *ToolResult {
	llmParts := make([]string, 0, len(content)+1)
	rawTextParts := make([]string, 0, len(content)+1)
	mediaRefs := make([]string, 0, len(content))

	for _, c := range content {
		switch v := c.(type) {
		case *mcp.TextContent:
			rawText := strings.TrimSpace(v.Text)
			if rawText != "" {
				rawTextParts = append(rawTextParts, rawText)
			}
			safeText := strings.TrimSpace(sanitizeToolLLMContent(v.Text))
			if safeText != "" {
				llmParts = append(llmParts, safeText)
			}
		case *mcp.ImageContent:
			ref, note := t.storeBinaryContent(
				ctx,
				"image",
				normalizedMIMEType(v.MIMEType),
				v.Data,
				v.Annotations,
			)
			if ref != "" {
				mediaRefs = append(mediaRefs, ref)
			}
			if note != "" {
				llmParts = append(llmParts, note)
			}
		case *mcp.AudioContent:
			ref, note := t.storeBinaryContent(
				ctx,
				"audio",
				normalizedMIMEType(v.MIMEType),
				v.Data,
				v.Annotations,
			)
			if ref != "" {
				mediaRefs = append(mediaRefs, ref)
			}
			if note != "" {
				llmParts = append(llmParts, note)
			}
		case *mcp.ResourceLink:
			llmParts = append(llmParts, summarizeResourceLink(v))
		case *mcp.EmbeddedResource:
			ref, note, rawText := t.storeEmbeddedResource(ctx, v)
			if ref != "" {
				mediaRefs = append(mediaRefs, ref)
			}
			if rawText != "" {
				rawTextParts = append(rawTextParts, rawText)
			}
			if note != "" {
				llmParts = append(llmParts, note)
			}
		default:
			llmParts = append(llmParts, fmt.Sprintf("[MCP 返回了不支持的内容类型 %T]", v))
		}
	}

	// MCP 2025-06-18+：structuredContent 是工具的结构化输出。把它以紧凑 JSON
	// 附加到模型可见结果（并纳入 rawText，从而复用大文本 artifact/spill 机制）。
	// 若服务端已把同样的 JSON 放进 Content 文本，则不重复追加。
	if llmPart, rawJSON := structuredContentLLMPart(structuredContent, strings.Join(compactStrings(llmParts), "\n")); llmPart != "" {
		llmParts = append(llmParts, llmPart)
		rawTextParts = append(rawTextParts, rawJSON)
	}

	forLLM := strings.Join(compactStrings(llmParts), "\n")
	rawText := strings.Join(compactStrings(rawTextParts), "\n")
	if artifactResult := t.persistLargeTextArtifact(rawText); artifactResult != nil {
		artifactResult.Media = mediaRefs
		return artifactResult
	}

	result := &ToolResult{
		ForLLM: forLLM,
		Media:  mediaRefs,
	}
	return result
}

// structuredContentLLMPart 将 MCP CallToolResult.StructuredContent 渲染为
// 紧凑的 JSON 片段，用于模型可见的结果。
//
// 当没有结构化内容、值无法序列化（会记录日志），或相同的紧凑 JSON 已经
// 出现在非结构化文本内容中时，它返回空字符串（将结构化输出重复放入
// Content 的服务器不得产生重复的模型上下文）。
func structuredContentLLMPart(value any, existingLLMText string) (llmPart, rawJSON string) {
	if value == nil {
		return "", ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		logger.WarnCF("tool", "Failed to marshal MCP structured content", map[string]any{
			"error": err.Error(),
		})
		return "", ""
	}
	compact := strings.TrimSpace(string(data))
	if compact == "" || compact == "null" {
		return "", ""
	}
	if existingLLMText != "" && strings.Contains(existingLLMText, compact) {
		return "", ""
	}
	return "[MCP 结构化内容] " + sanitizeToolLLMContent(compact), compact
}

func (t *MCPTool) persistLargeTextArtifact(text string) *ToolResult {
	text = strings.TrimSpace(text)
	limit := t.maxInlineTextRunes
	if limit <= 0 {
		limit = maxMCPInlineTextRunes
	}
	size := utf8.RuneCountInString(text)
	if text == "" || size <= limit || t.workspace == "" {
		return nil
	}

	dir := filepath.Join(t.workspace, ".artifacts", "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return t.largeTextArtifactFallback(text, err)
	}
	// TODO: 为 MCP artifact 文件增加生命周期清理/保留机制。

	pattern := fmt.Sprintf(
		"%s_%s_*.txt",
		sanitizeIdentifierComponent(t.serverName),
		sanitizeIdentifierComponent(t.tool.Name),
	)
	tmpFile, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return t.largeTextArtifactFallback(text, err)
	}
	path := tmpFile.Name()
	if _, err = tmpFile.WriteString(text); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(path)
		return t.largeTextArtifactFallback(text, err)
	}
	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(path)
		return t.largeTextArtifactFallback(text, err)
	}

	return &ToolResult{
		ForLLM: fmt.Sprintf(
			"[MCP 返回了较大的文本结果（%d 字符）；已从模型上下文省略并保存为本地 artifact。]",
			size,
		),
		ArtifactTags: []string{"[file:" + path + "]"},
	}
}

func (t *MCPTool) largeTextArtifactFallback(text string, err error) *ToolResult {
	size := utf8.RuneCountInString(text)
	logger.WarnCF("tool", "Failed to persist large MCP text artifact", map[string]any{
		"server": t.serverName,
		"tool":   t.tool.Name,
		"chars":  size,
		"error":  err.Error(),
	})
	return &ToolResult{
		ForLLM: fmt.Sprintf(
			"[MCP 返回了较大的文本结果（%d 字符）；因 artifact 落盘失败，已从模型上下文省略。]",
			size,
		),
	}
}

func (t *MCPTool) storeEmbeddedResource(ctx context.Context, content *mcp.EmbeddedResource) (string, string, string) {
	if content == nil || content.Resource == nil {
		return "", "[MCP 返回了缺少数据的嵌入资源。]", ""
	}

	resource := content.Resource
	if len(resource.Blob) > 0 {
		ref, note := t.storeBinaryContent(
			ctx,
			"resource",
			normalizedMIMEType(resource.MIMEType),
			resource.Blob,
			content.Annotations,
		)
		return ref, note, ""
	}

	rawText := strings.TrimSpace(resource.Text)
	if rawText != "" {
		return "", sanitizeToolLLMContent(resource.Text), rawText
	}

	return "", summarizeEmbeddedResource(content), ""
}

func (t *MCPTool) storeBinaryContent(
	ctx context.Context,
	kind string,
	mimeType string,
	data []byte,
	annotations *mcp.Annotations,
) (string, string) {
	if len(data) == 0 {
		return "", fmt.Sprintf("[MCP 返回了 %s 内容（%s），但内容为空。]", kind, mimeType)
	}
	if !annotationsAllowUser(annotations) {
		return "", fmt.Sprintf(
			"[MCP 返回了面向非 user 受众的 %s 内容（%s）；已从模型上下文省略。]",
			kind,
			mimeType,
		)
	}
	if t.mediaStore == nil {
		return "", fmt.Sprintf(
			"[MCP 返回了 %s 内容（%s）；因媒体投递不可用，已从模型上下文省略。]",
			kind,
			mimeType,
		)
	}

	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)
	if channel == "" || chatID == "" {
		return "", fmt.Sprintf(
			"[MCP 返回了 %s 内容（%s）；因没有可用的目标会话，已从模型上下文省略。]",
			kind,
			mimeType,
		)
	}

	dir := media.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Sprintf("[MCP 返回了 %s 内容（%s），但无法存储。]", kind, mimeType)
	}

	ext := extensionForMIMEType(mimeType)
	tmpFile, err := os.CreateTemp(dir, "mcp-*"+ext)
	if err != nil {
		return "", fmt.Sprintf("[MCP 返回了 %s 内容（%s），但无法存储。]", kind, mimeType)
	}
	tmpPath := tmpFile.Name()
	if _, err = tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[MCP 返回了 %s 内容（%s），但无法存储。]", kind, mimeType)
	}
	if err = tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf("[MCP 返回了 %s 内容（%s），但无法存储。]", kind, mimeType)
	}

	scope := fmt.Sprintf(
		"tool:mcp:%s:%s:%s:%d",
		sanitizeIdentifierComponent(t.serverName),
		channel,
		chatID,
		time.Now().UnixNano(),
	)
	filename := fmt.Sprintf(
		"%s_%s%s",
		sanitizeIdentifierComponent(t.serverName),
		sanitizeIdentifierComponent(t.tool.Name),
		ext,
	)

	ref, err := t.mediaStore.Store(tmpPath, media.MediaMeta{
		Filename:    filename,
		ContentType: mimeType,
		Source: fmt.Sprintf(
			"tool:mcp:%s:%s",
			sanitizeIdentifierComponent(t.serverName),
			sanitizeIdentifierComponent(t.tool.Name),
		),
	}, scope)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Sprintf(
			"[MCP 返回了 %s 内容（%s），但无法注册为媒体。]",
			kind,
			mimeType,
		)
	}

	return ref, fmt.Sprintf(
		"[MCP 返回了 %s 内容（%s）；已从模型上下文省略并存储为本地媒体 artifact。]",
		kind,
		mimeType,
	)
}

func summarizeResourceLink(content *mcp.ResourceLink) string {
	if content == nil {
		return "[MCP 返回了空的资源链接。]"
	}

	parts := []string{"[MCP 返回资源链接"}
	if content.Name != "" {
		parts = append(parts, fmt.Sprintf("名称=%q", content.Name))
	}
	if content.URI != "" {
		parts = append(parts, fmt.Sprintf("uri=%q", content.URI))
	}
	if content.MIMEType != "" {
		parts = append(parts, fmt.Sprintf("mime=%q", content.MIMEType))
	}
	if content.Description != "" {
		desc := strings.TrimSpace(content.Description)
		if len(desc) > 200 {
			desc = desc[:200] + "..."
		}
		parts = append(parts, fmt.Sprintf("描述=%q", desc))
	}
	return strings.Join(parts, ", ") + "]"
}

func summarizeEmbeddedResource(content *mcp.EmbeddedResource) string {
	if content == nil || content.Resource == nil {
		return "[MCP 返回了嵌入资源。]"
	}

	resource := content.Resource
	if resource.URI != "" {
		return fmt.Sprintf(
			"[MCP 返回嵌入资源 %q（%s）。]",
			resource.URI,
			normalizedMIMEType(resource.MIMEType),
		)
	}
	return fmt.Sprintf("[MCP 返回嵌入资源（%s）。]", normalizedMIMEType(resource.MIMEType))
}

func annotationsAllowUser(annotations *mcp.Annotations) bool {
	if annotations == nil || len(annotations.Audience) == 0 {
		return true
	}
	for _, audience := range annotations.Audience {
		if strings.EqualFold(string(audience), "user") {
			return true
		}
	}
	return false
}

func normalizedMIMEType(mimeType string) string {
	if strings.TrimSpace(mimeType) == "" {
		return "application/octet-stream"
	}
	return mimeType
}

func compactStrings(parts []string) []string {
	compact := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		compact = append(compact, part)
	}
	return compact
}
