package mcpresources

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

const maxDescriptionRunes = 300

// -----------------------------------------------------------------------------
// list_mcp_resources
// -----------------------------------------------------------------------------

// ListResourcesTool 实现 `list_mcp_resources`（MCP resources/list）。
type ListResourcesTool struct {
	rt *resourceTools
}

// NewListResourcesTool 基于给定的 MCP manager 接口构造该工具。
func NewListResourcesTool(manager Manager, opts ...Option) *ListResourcesTool {
	return &ListResourcesTool{rt: newResourceTools(manager, opts...)}
}

func (t *ListResourcesTool) Name() string { return ToolListResources }

func (t *ListResourcesTool) Description() string {
	return "列出已连接 MCP server 上可读取的 resources（资源 URI/名称/MIME/大小）。" +
		"server 省略时聚合所有已连接 server；cursor 用于分页。只读，不修改任何状态。"
}

func (t *ListResourcesTool) Parameters() map[string]any {
	return listResourcesParameters(ToolListResources)
}

func (t *ListResourcesTool) PromptMetadata() toolshared.PromptMetadata {
	return promptMetadata()
}

func (t *ListResourcesTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	serverName := argString(args, "server")
	cursor := argString(args, "cursor")

	names, err := t.rt.listTargets(serverName, cursor)
	if err != nil {
		return toolshared.ErrorResult(err.Error()).WithError(err)
	}

	sections := make([]string, 0, len(names))
	successes := 0
	total := 0
	truncated := false

	for _, name := range names {
		session, err := t.rt.session(name)
		if err != nil {
			sections = append(sections, fmt.Sprintf("MCP server：%s\n[错误] %v", name, err))
			continue
		}
		result, err := session.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
		if err != nil {
			sections = append(sections, fmt.Sprintf("MCP server：%s\n[错误] %v", name, err))
			continue
		}
		successes++

		var b strings.Builder
		fmt.Fprintf(&b, "MCP server：%s", name)
		if result.NextCursor != "" {
			fmt.Fprintf(&b, "\nnext_cursor: %s", result.NextCursor)
		}
		items := 0
		for _, resource := range result.Resources {
			if resource == nil {
				continue
			}
			if total >= t.rt.opts.maxListItems {
				truncated = true
				break
			}
			total++
			items++
			appendResourceSummary(&b, resource)
		}
		if items == 0 && !truncated {
			b.WriteString("\n(no resources)")
		}
		sections = append(sections, b.String())
	}

	if successes == 0 {
		joined := strings.Join(sections, "\n\n")
		return toolshared.ErrorResult(joined).
			WithError(fmt.Errorf("所有 MCP server 都未能列出 resources"))
	}
	if truncated {
		sections = append(sections, fmt.Sprintf(
			"[已截断：最多只展示 %d 个 resource；可传 `server` 或 `cursor` 缩小范围]",
			t.rt.opts.maxListItems,
		))
	}
	return toolshared.SilentResult(strings.Join(sections, "\n\n"))
}

func appendResourceSummary(b *strings.Builder, resource *mcp.Resource) {
	fmt.Fprintf(b, "\n- uri: %s", resource.URI)
	if resource.Name != "" {
		fmt.Fprintf(b, "\n  名称: %s", resource.Name)
	}
	if resource.Title != "" {
		fmt.Fprintf(b, "\n  标题: %s", resource.Title)
	}
	if resource.MIMEType != "" {
		fmt.Fprintf(b, "\n  mime_type: %s", normalizedMIMEType(resource.MIMEType))
	}
	if resource.Size > 0 {
		fmt.Fprintf(b, "\n  大小: %d 字节", resource.Size)
	}
	if resource.Description != "" {
		fmt.Fprintf(b, "\n  描述: %s", textPreview(resource.Description, maxDescriptionRunes))
	}
}

// -----------------------------------------------------------------------------
// list_mcp_resource_templates
// -----------------------------------------------------------------------------

// ListResourceTemplatesTool 实现 `list_mcp_resource_templates`
// （MCP resources/templates/list）。
type ListResourceTemplatesTool struct {
	rt *resourceTools
}

// NewListResourceTemplatesTool 基于给定的 MCP manager 接口构造该工具。
func NewListResourceTemplatesTool(manager Manager, opts ...Option) *ListResourceTemplatesTool {
	return &ListResourceTemplatesTool{rt: newResourceTools(manager, opts...)}
}

func (t *ListResourceTemplatesTool) Name() string { return ToolListResourceTemplates }

func (t *ListResourceTemplatesTool) Description() string {
	return "列出已连接 MCP server 的 parameterized resource URI 模板（RFC 6570）。" +
		"用 list_mcp_resources 查看实例化后的具体 URI；模板可用 " + ToolReadResource + " 展开读取。"
}

func (t *ListResourceTemplatesTool) Parameters() map[string]any {
	return listResourcesParameters(ToolListResourceTemplates)
}

func (t *ListResourceTemplatesTool) PromptMetadata() toolshared.PromptMetadata {
	return promptMetadata()
}

func (t *ListResourceTemplatesTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	serverName := argString(args, "server")
	cursor := argString(args, "cursor")

	names, err := t.rt.listTargets(serverName, cursor)
	if err != nil {
		return toolshared.ErrorResult(err.Error()).WithError(err)
	}

	sections := make([]string, 0, len(names))
	successes := 0
	total := 0
	truncated := false

	for _, name := range names {
		session, err := t.rt.session(name)
		if err != nil {
			sections = append(sections, fmt.Sprintf("MCP server：%s\n[错误] %v", name, err))
			continue
		}
		result, err := session.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
		if err != nil {
			sections = append(sections, fmt.Sprintf("MCP server：%s\n[错误] %v", name, err))
			continue
		}
		successes++

		var b strings.Builder
		fmt.Fprintf(&b, "MCP server: %s", name)
		if result.NextCursor != "" {
			fmt.Fprintf(&b, "\nnext_cursor: %s", result.NextCursor)
		}
		items := 0
		for _, template := range result.ResourceTemplates {
			if template == nil {
				continue
			}
			if total >= t.rt.opts.maxListItems {
				truncated = true
				break
			}
			total++
			items++
			fmt.Fprintf(&b, "\n- uri_template: %s", template.URITemplate)
			if template.Name != "" {
				fmt.Fprintf(&b, "\n  名称: %s", template.Name)
			}
			if template.Title != "" {
				fmt.Fprintf(&b, "\n  标题: %s", template.Title)
			}
			if template.MIMEType != "" {
				fmt.Fprintf(&b, "\n  mime_type: %s", normalizedMIMEType(template.MIMEType))
			}
			if template.Description != "" {
				fmt.Fprintf(&b, "\n  描述: %s", textPreview(template.Description, maxDescriptionRunes))
			}
		}
		if items == 0 && !truncated {
			b.WriteString("\n（没有 resource template）")
		}
		sections = append(sections, b.String())
	}

	if successes == 0 {
		joined := strings.Join(sections, "\n\n")
		return toolshared.ErrorResult(joined).
			WithError(fmt.Errorf("所有 MCP server 都未能列出 resource templates"))
	}
	if truncated {
		sections = append(sections, fmt.Sprintf(
			"[已截断：最多只展示 %d 个 resource template；可传 `server` 或 `cursor` 缩小范围]",
			t.rt.opts.maxListItems,
		))
	}
	return toolshared.SilentResult(strings.Join(sections, "\n\n"))
}

// -----------------------------------------------------------------------------
// read_mcp_resource
// -----------------------------------------------------------------------------

// ReadResourceTool 实现 `read_mcp_resource`（MCP resources/read）。
type ReadResourceTool struct {
	rt *resourceTools
}

// NewReadResourceTool 基于给定的 MCP manager 接口构造该工具。
func NewReadResourceTool(manager Manager, opts ...Option) *ReadResourceTool {
	return &ReadResourceTool{rt: newResourceTools(manager, opts...)}
}

func (t *ReadResourceTool) Name() string { return ToolReadResource }

func (t *ReadResourceTool) Description() string {
	return "按 URI 读取 MCP resource 的内容。文本内联返回；过长的文本或二进制内容会落盘为 local artifact 并返回路径" +
		"（二进制不会进入模型上下文）。只读操作。"
}

func (t *ReadResourceTool) Parameters() map[string]any {
	return readResourceParameters()
}

func (t *ReadResourceTool) PromptMetadata() toolshared.PromptMetadata {
	return promptMetadata()
}

func (t *ReadResourceTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	uri := argString(args, "uri")
	if uri == "" {
		return toolshared.ErrorResult(ToolReadResource + " 需要非空的 `uri` 参数")
	}
	serverName := argString(args, "server")

	name, session, err := t.rt.readTarget(serverName)
	if err != nil {
		return toolshared.ErrorResult(err.Error()).WithError(err)
	}

	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return toolshared.ErrorResult(fmt.Sprintf(
			"MCP server %q 读取 resource %q 失败：%v", name, uri, err,
		)).WithError(err)
	}
	if result == nil || len(result.Contents) == 0 {
		return toolshared.ErrorResult(fmt.Sprintf(
			"MCP server %q 对 resource %q 未返回任何内容", name, uri,
		))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "MCP server：%s\nuri: %s", name, uri)
	artifactTags := make([]string, 0, len(result.Contents))

	for i, content := range result.Contents {
		if content == nil {
			continue
		}
		contentURI := content.URI
		if contentURI == "" {
			contentURI = uri
		}
		mimeType := normalizedMIMEType(content.MIMEType)
		fmt.Fprintf(&b, "\ncontent[%d]: uri=%s mime_type=%s", i, contentURI, mimeType)

		if len(content.Blob) > 0 {
			if int64(len(content.Blob)) > t.rt.opts.maxReadBytes {
				return toolshared.ErrorResult(fmt.Sprintf(
					"MCP resource %q 的 content[%d] 为 %d 字节，超过配置上限 %d 字节",
					uri, i, len(content.Blob), t.rt.opts.maxReadBytes,
				))
			}
			path, err := t.rt.persistArtifact(name, "blob", extensionForMIMEType(mimeType), content.Blob)
			if err != nil {
				fmt.Fprintf(&b, "\n  [二进制资源：%d 字节；artifact 落盘失败：%v]", len(content.Blob), err)
				continue
			}
			fmt.Fprintf(&b, "\n  [二进制资源：%d 字节，已保存至 %s]", len(content.Blob), path)
			artifactTags = append(artifactTags, "[file:"+path+"]")
			continue
		}

		text := content.Text
		if text == "" {
			b.WriteString("\n  （内容为空）")
			continue
		}
		if utf8.RuneCountInString(text) <= t.rt.opts.maxInlineTextRunes {
			fmt.Fprintf(&b, "\n%s", text)
			continue
		}
		path, err := t.rt.persistArtifact(name, "text", ".txt", []byte(text))
		if err != nil {
			fmt.Fprintf(&b, "\n  [大文本（%d 字符）已从模型上下文省略：artifact 落盘失败：%v]",
				utf8.RuneCountInString(text), err)
			continue
		}
		fmt.Fprintf(&b,
			"\n  [大文本（%d 字符）；已从模型上下文省略，并保存为本地 artifact：%s]",
			utf8.RuneCountInString(text), path,
		)
		artifactTags = append(artifactTags, "[file:"+path+"]")
	}

	out := toolshared.SilentResult(strings.TrimSpace(b.String()))
	out.ArtifactTags = artifactTags
	return out
}

// -----------------------------------------------------------------------------
// 注册
// -----------------------------------------------------------------------------

// Register 在给定 registry 上注册全部三个 MCP resource 工具。
// agent 层只需在连接 MCP manager 之后调用一次：
//
//	mcpresources.Register(agent.Tools, mcpManager, mcpresources.WithWorkspace(agent.Workspace))
//
// registry 为 nil 时注册为空操作。
func Register(r *tools.ToolRegistry, manager Manager, opts ...Option) {
	if r == nil {
		return
	}
	r.Register(NewListResourcesTool(manager, opts...))
	r.Register(NewListResourceTemplatesTool(manager, opts...))
	r.Register(NewReadResourceTool(manager, opts...))
}
