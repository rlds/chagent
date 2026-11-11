// session_search：在 workspace 会话历史中检索关键词/正则。
//
// 对应 DSH `packages/experimental/tool-session-query` 的 `session_search` 工具，
// 但适配 chagent 的 `workspace/sessions/*.jsonl` 持久化格式：DSH 按“每会话一条最佳
// 事件命中”返回；chagent 没有事件投影，因此按“每条消息命中”返回会话、回合、文件
// 行号、角色与摘要片段。

package sessionquery

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools/shared"
)

// SearchTool 实现 `session_search`。
type SearchTool struct {
	workspace string
	opts      Options
}

// NewSearchTool 用默认上限构造 session_search 工具。
func NewSearchTool(workspace string) *SearchTool {
	return NewSearchToolWithOptions(workspace, DefaultOptions())
}

// NewSearchToolWithOptions 用自定义上限构造 session_search 工具（零值上限回落默认）。
func NewSearchToolWithOptions(workspace string, opts Options) *SearchTool {
	return &SearchTool{workspace: workspace, opts: opts.normalized()}
}

// Name 返回稳定工具名。
func (t *SearchTool) Name() string { return SearchToolName }

// Description 返回模型可见的中文工具描述。
func (t *SearchTool) Description() string {
	return "在 workspace 的会话历史（sessions/*.jsonl）中检索关键词或正则，" +
		"返回会话、回合、文件行号、角色与摘要片段。只读且不跟随符号链接；" +
		"默认检索当前会话，session=\"*\" 检索全部会话。"
}

// PromptMetadata 返回 DSH 工具组的提示元数据。
func (t *SearchTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// ConcurrencySafe 声明该工具只读，可与其他只读工具并行执行。
func (t *SearchTool) ConcurrencySafe() bool { return true }

// Parameters 返回模型参数 schema（中文描述）。
func (t *SearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "必填：要检索的关键词（字面子串匹配；regex=true 时按 Go 正则解释）",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"description": "可选：true 时把 query 当作 Go 正则表达式，默认 false（字面匹配）",
			},
			"case_sensitive": map[string]any{
				"type":        "boolean",
				"description": "可选：true 时区分大小写，默认 false",
			},
			"session": map[string]any{
				"type": "string",
				"description": "可选：目标会话 key（也接受 sessions 文件名，可不含 .jsonl）；" +
					"缺省检索当前会话，\"*\" 检索全部会话",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("可选：最多返回多少条命中，默认 %d，上限 %d", DefaultLimit, MaxLimit),
			},
		},
		"required": []string{"query"},
	}
}

// searchHit 是一条检索命中。
type searchHit struct {
	SessionKey string
	Turn       int
	RelPath    string
	Line       int
	Role       string
	Snippet    string
}

// Execute 执行检索。
func (t *SearchTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	if t == nil {
		return toolshared.ErrorResult("session_search 失败: 工具未初始化")
	}
	opts := t.opts

	query, present, err := stringArg(args, "query")
	if err != nil {
		return toolError(SearchToolName, err)
	}
	if !present {
		return toolshared.ErrorResult(`session_search 失败: 缺少必填参数 "query"`)
	}
	if err := validateQuery(query, opts.MaxQueryRunes); err != nil {
		return toolError(SearchToolName, err)
	}
	useRegex, err := boolArg(args, "regex", false)
	if err != nil {
		return toolError(SearchToolName, err)
	}
	caseSensitive, err := boolArg(args, "case_sensitive", false)
	if err != nil {
		return toolError(SearchToolName, err)
	}
	sessionArg, _, err := stringArg(args, "session")
	if err != nil {
		return toolError(SearchToolName, err)
	}
	limitArg, limitPresent, err := intArg(args, "limit")
	if err != nil {
		return toolError(SearchToolName, err)
	}
	limit, clamped, err := opts.resolveLimit(limitArg, limitPresent)
	if err != nil {
		return toolError(SearchToolName, err)
	}

	match, err := newQueryMatcher(query, useRegex, caseSensitive)
	if err != nil {
		return toolError(SearchToolName, err)
	}

	selector, scopeNote := resolveDefaultSessionSelector(ctx, sessionArg)
	discovery, listErr := listSessions(t.workspace)
	if listErr != nil {
		return toolError(SearchToolName, listErr)
	}
	sessions := filterSessions(discovery.Sessions, selector)
	explicitSelector := strings.TrimSpace(sessionArg) != "" && selector != "*"
	if explicitSelector && len(sessions) == 0 {
		return toolshared.ErrorResult(fmt.Sprintf(
			"session_search 失败: 未找到会话 %q（已发现 %d 个会话文件；可用 session=\"*\" 检索全部）",
			selector, len(discovery.Sessions),
		))
	}

	scanCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	diags := scanDiagnostics{FilesSkipped: discovery.SkippedLinks}
	if len(discovery.BadMetaKeys) > 0 {
		diags.UnreadableFiles = append(diags.UnreadableFiles,
			"meta 无法解析（已按文件名识别）: "+strings.Join(discovery.BadMetaKeys, ", "))
	}

	hits := make([]searchHit, 0, limit)
	capped := false
	scannedFiles := 0
	for _, sf := range sessions {
		if len(hits) >= limit {
			capped = true
			break
		}
		if scannedFiles >= opts.MaxFiles {
			diags.HitFileLimit = true
			break
		}
		scannedFiles++
		turn := 0
		scanErr := scanSessionFile(scanCtx, sf, opts, &diags, func(line scanLine) bool {
			if err := scanCtx.Err(); err != nil {
				diags.HitTimeLimit = true
				return false
			}
			if isTurnBoundaryMessage(line.Msg) {
				turn++
			}
			text := messageSearchText(line.Msg)
			matched, matchedText := match(text)
			if !matched {
				return true
			}
			snippet := snippetAround(text, matchedText, opts.MaxSnippetRunes)
			hits = append(hits, searchHit{
				SessionKey: sf.Key,
				Turn:       turn,
				RelPath:    sf.RelPath,
				Line:       line.Line,
				Role:       roleLabel(line.Msg.Role),
				Snippet:    snippet,
			})
			return len(hits) < limit
		})
		if scanErr != nil {
			// 单文件读取失败不终止整次检索：诊断已记录，继续其他文件。
			continue
		}
		if diags.HitTimeLimit || diags.HitByteLimit {
			break
		}
	}

	if diags.HitTimeLimit {
		return toolshared.ErrorResult(fmt.Sprintf(
			"session_search 失败: 扫描超时（上限 %s），已命中 %d 条但结果不完整；"+
				"请缩小 query 或指定 session。",
			opts.Timeout, len(hits),
		))
	}

	return toolshared.SilentResult(renderSearchResult(query, useRegex, caseSensitive, selector, scopeNote, hits, limit, capped, clamped, diags))
}

// validateQuery 校验查询串：非空、无 NUL、长度受限。
func validateQuery(query string, maxRunes int) error {
	if strings.TrimSpace(query) == "" {
		return &argError{Field: "query", Message: "不能为空白"}
	}
	if strings.ContainsRune(query, '\x00') {
		return &argError{Field: "query", Message: "不能包含 NUL 字符"}
	}
	if maxRunes > 0 {
		if runes := []rune(query); len(runes) > maxRunes {
			return &argError{Field: "query", Message: fmt.Sprintf("过长（%d > 上限 %d 字符）", len(runes), maxRunes)}
		}
	}
	return nil
}

// newQueryMatcher 构造字面/正则匹配器；返回 (是否命中, 实际命中文本)。
func newQueryMatcher(query string, useRegex, caseSensitive bool) (func(text string) (bool, string), error) {
	if useRegex {
		pattern := query
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, &argError{Field: "query", Message: "正则表达式无效: " + err.Error()}
		}
		return func(text string) (bool, string) {
			loc := compiled.FindStringIndex(text)
			if loc == nil {
				return false, ""
			}
			return true, text[loc[0]:loc[1]]
		}, nil
	}

	needle := query
	if !caseSensitive {
		needle = strings.ToLower(query)
	}
	return func(text string) (bool, string) {
		haystack := text
		if !caseSensitive {
			haystack = strings.ToLower(text)
		}
		if !strings.Contains(haystack, needle) {
			return false, ""
		}
		return true, query
	}, nil
}

// messageSearchText 返回一条消息的可检索文本：正文 + 工具调用名与参数。
func messageSearchText(msg providers.Message) string {
	var b strings.Builder
	b.WriteString(msg.Content)
	for _, tc := range msg.ToolCalls {
		name := tc.Name
		args := ""
		if tc.Function != nil {
			if name == "" {
				name = tc.Function.Name
			}
			args = tc.Function.Arguments
		}
		if name != "" || args != "" {
			b.WriteString(" ")
			b.WriteString(name)
			b.WriteString(" ")
			b.WriteString(args)
		}
	}
	return b.String()
}

// roleLabel 归一化角色标签（空角色用 "-" 表示）。
func roleLabel(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "-"
	}
	return role
}

// renderSearchResult 渲染模型可读结果。
func renderSearchResult(
	query string,
	useRegex, caseSensitive bool,
	selector, scopeNote string,
	hits []searchHit,
	limit int,
	capped, clamped bool,
	diags scanDiagnostics,
) string {
	mode := "literal"
	if useRegex {
		mode = "regex"
	}
	caseLabel := "insensitive"
	if caseSensitive {
		caseLabel = "sensitive"
	}
	lines := []string{fmt.Sprintf(
		"session_search: query=%q mode=%s case=%s sessions=%s hits=%d limit=%d",
		maskSensitiveText(singleLine(query)), mode, caseLabel, maskSensitiveText(selector), len(hits), limit,
	)}
	for _, hit := range hits {
		lines = append(lines, fmt.Sprintf(
			"- session=%s turn=%d %s:%d %s: %s",
			maskSensitiveText(hit.SessionKey), hit.Turn, hit.RelPath, hit.Line, hit.Role, hit.Snippet,
		))
	}
	if len(hits) == 0 {
		lines = append(lines, "", "未找到匹配。可尝试：放宽 query、关闭 case_sensitive、"+
			"使用 regex=true，或用 session=\"*\" 检索全部会话。")
	}
	if capped || clamped {
		lines = append(lines, "", fmt.Sprintf("结果数已达到 limit=%d 上限，可能还有未返回的匹配。", limit))
	}
	if scopeNote != "" {
		lines = append(lines, "", "说明: "+scopeNote)
	}
	lines = appendDiagnostics(lines, diags)
	return strings.Join(lines, "\n")
}
