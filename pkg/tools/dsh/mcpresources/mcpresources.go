// Package mcpresources 实现 DSH 工具组 3：基于 pkg/mcp 已管理连接会话的
// 只读 MCP 资源工具。
//
// 这三个工具对应 DSH 参考实现
// (packages/mcp/mcp-resources/src/tools.ts)：
//
//	list_mcp_resources           resources/list
//	list_mcp_resource_templates  resources/templates/list
//	read_mcp_resource            resources/read
//
// 此处所有操作均为只读。二进制负载绝不内联进模型上下文：
// 它们会被持久化到 <workspace>/.artifacts/mcp-resources/（未配置工作区时
// 写到临时目录），并以路径形式引用。
package mcpresources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	picomcp "github.com/rlds/chagent/pkg/mcp"
	"github.com/rlds/chagent/pkg/media"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// 工具名遵循 DSH 的命名约定。
const (
	ToolListResources         = "list_mcp_resources"
	ToolListResourceTemplates = "list_mcp_resource_templates"
	ToolReadResource          = "read_mcp_resource"

	// DSH 工具组的 PromptMetadata 约定。
	promptLayer  = "capability"
	promptSlot   = "dsh"
	promptSource = "tool_registry:dsh"
)

// 默认限制。均可通过选项覆盖。
const (
	defaultMaxListItems       = 200
	defaultMaxInlineTextRunes = 16 * 1024
	defaultMaxReadBytes       = 1 << 20 // 每个二进制内容块 1 MiB
)

// Manager 是这些工具所使用的 *pkg/mcp.Manager 的窄接口。
// *pkg/mcp.Manager 直接实现了它（无需适配器），
// 因此 agent 层只需一次 Register 调用即可完成接线。
type Manager interface {
	// GetServers 按名称返回所有当前已连接的 server。
	GetServers() map[string]*picomcp.ServerConnection
}

// 编译期检查：生产的 Manager 满足该窄接口。
var _ Manager = (*picomcp.Manager)(nil)

// options 是各工具组的限制与 artifact 目标位置。
type options struct {
	workspace          string
	maxListItems       int
	maxInlineTextRunes int
	maxReadBytes       int64
}

// Option 用于定制资源工具的行为。
type Option func(*options)

// WithWorkspace 设置用于存放持久化资源 artifact 的工作区根目录
// （<workspace>/.artifacts/mcp-resources/）。为空时 artifact 改为写入临时目录。
func WithWorkspace(path string) Option {
	return func(o *options) { o.workspace = strings.TrimSpace(path) }
}

// WithMaxListItems 限制单次调用聚合的 resource/template 数量。
func WithMaxListItems(limit int) Option {
	return func(o *options) {
		if limit > 0 {
			o.maxListItems = limit
		}
	}
}

// WithMaxInlineTextRunes 设置内联文本阈值；超长文本内容会被持久化为
// artifact，在模型上下文中只给出摘要。
func WithMaxInlineTextRunes(limit int) Option {
	return func(o *options) {
		if limit > 0 {
			o.maxInlineTextRunes = limit
		}
	}
}

// WithMaxReadBytes 限制单个二进制内容块的大小；更大的数据块会被
// 拒绝并返回诊断错误，而不会加载或持久化。
func WithMaxReadBytes(limit int64) Option {
	return func(o *options) {
		if limit > 0 {
			o.maxReadBytes = limit
		}
	}
}

func newOptions(opts ...Option) options {
	o := options{
		maxListItems:       defaultMaxListItems,
		maxInlineTextRunes: defaultMaxInlineTextRunes,
		maxReadBytes:       defaultMaxReadBytes,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// resourceTools 保存这三个工具共享的状态。
type resourceTools struct {
	manager Manager
	opts    options
}

func newResourceTools(manager Manager, opts ...Option) *resourceTools {
	return &resourceTools{manager: manager, opts: newOptions(opts...)}
}

// connectedServers 返回已连接 server 的排序名称列表。
func (rt *resourceTools) connectedServers() ([]string, error) {
	if rt == nil || rt.manager == nil {
		return nil, fmt.Errorf("MCP manager 不可用")
	}
	servers := rt.manager.GetServers()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("当前没有已连接的 MCP server；请先配置并启用 tools.mcp.servers")
	}
	return names, nil
}

// session 返回指定 server 名称对应的已连接会话。
func (rt *resourceTools) session(serverName string) (*mcp.ClientSession, error) {
	servers := rt.manager.GetServers()
	conn, ok := servers[serverName]
	if !ok || conn == nil {
		return nil, fmt.Errorf(
			"MCP server %q 未连接（配置的 server 必须已启用且连接成功；可用 /list mcp 查看）",
			serverName,
		)
	}
	if conn.Session == nil {
		return nil, fmt.Errorf("MCP server %q 没有活动会话；请重新连接该 server 后重试", serverName)
	}
	return conn.Session, nil
}

// listTargets 解析列表操作应查询的 server。
//
//   - 指定 server：只查询该 server。
//   - server 为空：查询所有已连接的 server（聚合结果）。
//   - 设置 cursor 时要求只有一个明确的目标 server。
func (rt *resourceTools) listTargets(serverName, cursor string) ([]string, error) {
	names, err := rt.connectedServers()
	if err != nil {
		return nil, err
	}
	if serverName != "" {
		if _, err := rt.session(serverName); err != nil {
			return nil, err
		}
		return []string{serverName}, nil
	}
	if cursor != "" && len(names) > 1 {
		return nil, fmt.Errorf(
			"连接了多个 MCP server 时，cursor 分页必须显式指定 `server`（已连接：%s）",
			strings.Join(names, ", "),
		)
	}
	return names, nil
}

// readTarget 解析读取操作应使用的唯一 server。
//
//   - 指定 server：只使用该 server。
//   - server 为空且只连接了一个 server：使用该 server。
//   - server 为空且连接了多个 server：返回指明歧义的错误。
func (rt *resourceTools) readTarget(serverName string) (string, *mcp.ClientSession, error) {
	names, err := rt.connectedServers()
	if err != nil {
		return "", nil, err
	}
	if serverName != "" {
		session, err := rt.session(serverName)
		return serverName, session, err
	}
	if len(names) > 1 {
		return "", nil, fmt.Errorf(
			"连接了多个 MCP server 时，read_mcp_resource 必须显式指定 `server`（已连接：%s）",
			strings.Join(names, ", "),
		)
	}
	session, err := rt.session(names[0])
	return names[0], session, err
}

// artifactDir 返回用于存放持久化资源负载的目录。
func (rt *resourceTools) artifactDir() (string, error) {
	var dir string
	if rt.opts.workspace != "" {
		dir = filepath.Join(rt.opts.workspace, ".artifacts", "mcp-resources")
	} else {
		dir = filepath.Join(media.TempDir(), "mcp-resources")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建 MCP resource artifact 目录失败：%w", err)
	}
	return dir, nil
}

// persistArtifact 将资源负载字节写入 artifact 文件并返回其路径。
// Artifact 文件权限为 0600，位于 0700 的目录中。
func (rt *resourceTools) persistArtifact(server, kind, ext string, data []byte) (string, error) {
	dir, err := rt.artifactDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	name := fmt.Sprintf(
		"%s_%s_%s%s",
		sanitizeName(server),
		sanitizeName(kind),
		hex.EncodeToString(sum[:6]),
		ext,
	)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("写入 MCP resource artifact 失败：%w", err)
	}
	return path, nil
}

// sanitizeName 规范化名称，使其可用于 artifact 文件名。
func sanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "unnamed"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// extensionForMIMEType 为持久化的二进制负载选择文件扩展名。
func extensionForMIMEType(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(strings.Split(strings.Split(mimeType, ";")[0], " ")[0]))
	switch mimeType {
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/html":
		return ".html"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "application/octet-stream":
		return ".bin"
	case "":
		return ".bin"
	}
	if strings.HasPrefix(mimeType, "text/") {
		return ".txt"
	}
	if i := strings.LastIndex(mimeType, "/"); i >= 0 && i+1 < len(mimeType) {
		sub := sanitizeName(mimeType[i+1:])
		if sub != "unnamed" {
			return "." + sub
		}
	}
	return ".bin"
}

func normalizedMIMEType(mimeType string) string {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}

// argString 读取可选的字符串参数并去除首尾空白。
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// textPreview 为超大负载生成有长度上限、对模型安全的预览。
func textPreview(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + fmt.Sprintf("\n[……已截断，共 %d 字符]", utf8.RuneCountInString(text))
}

func promptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  promptLayer,
		Slot:   promptSlot,
		Source: promptSource,
	}
}

// listResourcesParameters 是两个 list 工具共用的 JSON schema。
func listResourcesParameters(toolName string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "MCP server 名称；省略时聚合所有已连接的 server。",
			},
			"cursor": map[string]any{
				"type":        "string",
				"description": "上一次 " + toolName + " 返回的分页 cursor；多 server 时必须显式指定 server。",
			},
		},
		"required": []string{},
	}
}

// readResourceParameters 是 read_mcp_resource 的 JSON schema。
func readResourceParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "MCP server 名称；仅连接一个 server 时可省略。",
			},
			"uri": map[string]any{
				"type":        "string",
				"description": "要读取的资源 URI（来自 list_mcp_resources 或展开 resources 模板）。",
			},
		},
		"required": []string{"uri"},
	}
}
