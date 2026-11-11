package sessionquery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// TestToolMetadataAndSchema 锁定两个工具的名称/描述/PromptMetadata/参数 schema。
func TestToolMetadataAndSchema(t *testing.T) {
	search := NewSearchTool(t.TempDir())
	trace := NewTraceTool(t.TempDir())

	if got := search.Name(); got != SearchToolName {
		t.Fatalf("SearchTool.Name() = %q, want %q", got, SearchToolName)
	}
	if got := trace.Name(); got != TraceToolName {
		t.Fatalf("TraceTool.Name() = %q, want %q", got, TraceToolName)
	}
	for name, description := range map[string]string{
		"session_search": search.Description(),
		"session_trace":  trace.Description(),
	} {
		if !strings.Contains(description, "会话") {
			t.Errorf("%s description 不是中文/未提到会话: %q", name, description)
		}
	}

	wantMeta := toolshared.PromptMetadata{
		Layer:  "capability",
		Slot:   "dsh",
		Source: "tool_registry:dsh",
	}
	if got := search.PromptMetadata(); got != wantMeta {
		t.Fatalf("SearchTool.PromptMetadata() = %+v, want %+v", got, wantMeta)
	}
	if got := trace.PromptMetadata(); got != wantMeta {
		t.Fatalf("TraceTool.PromptMetadata() = %+v, want %+v", got, wantMeta)
	}
	if !search.ConcurrencySafe() || !trace.ConcurrencySafe() {
		t.Fatal("只读会话工具必须声明 ConcurrencySafe()=true")
	}

	searchParams := search.Parameters()
	props, ok := searchParams["properties"].(map[string]any)
	if !ok {
		t.Fatalf("search Parameters().properties 类型 = %T", searchParams["properties"])
	}
	for _, key := range []string{"query", "regex", "case_sensitive", "session", "limit"} {
		if _, ok := props[key]; !ok {
			t.Errorf("session_search 缺少参数 %q", key)
		}
	}
	required, ok := searchParams["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("session_search required = %#v, want [query]", searchParams["required"])
	}

	traceParams := trace.Parameters()
	traceProps, ok := traceParams["properties"].(map[string]any)
	if !ok {
		t.Fatalf("trace Parameters().properties 类型 = %T", traceParams["properties"])
	}
	for _, key := range []string{"session", "turn", "limit"} {
		if _, ok := traceProps[key]; !ok {
			t.Errorf("session_trace 缺少参数 %q", key)
		}
	}
}

// TestSearchLiteralAcrossAllSessions 覆盖字面检索、跨会话与结果行格式。
func TestSearchLiteralAcrossAllSessions(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewSearchTool(workspace)

	result := tool.Execute(context.Background(), map[string]any{
		"query":   "bug-123",
		"session": "*",
	})
	if result.IsError {
		t.Fatalf("session_search 返回错误: %s", resultContent(result))
	}
	text := resultContent(result)
	// 命中 4 个会话（demo/other/skip/corrupt）+ badmeta；符号链接与目录外文件不命中。
	for _, session := range []string{
		"agent:main:demo", "agent:main:other", "agent:main:skip",
		"agent:main:corrupt", "agent_main_badmeta",
	} {
		if !strings.Contains(text, "session="+session) {
			t.Errorf("缺少会话 %q 的命中:\n%s", session, text)
		}
	}
	if strings.Contains(text, "outside-secret") || strings.Contains(text, "outside-root-marker") {
		t.Fatalf("检索越界读到了 sessions 目录外的内容:\n%s", text)
	}
	// 行格式必须包含 `path:line role: snippet`。
	if !strings.Contains(text, "sessions/agent_main_demo.jsonl:2 user: ") {
		t.Errorf("命中行缺少 path:line role: snippet 格式:\n%s", text)
	}
	// 第一回合从真实用户消息开始：demo 的第 2 行属于 turn=1。
	if !strings.Contains(text, "session=agent:main:demo turn=1 sessions/agent_main_demo.jsonl:2") {
		t.Errorf("命中行缺少 session/turn 信息:\n%s", text)
	}
	if !strings.Contains(text, `mode=literal`) || !strings.Contains(text, `case=insensitive`) {
		t.Errorf("结果头缺少 mode/case 说明:\n%s", text)
	}
}

// TestSearchDefaultCurrentSession 覆盖缺省 session=当前会话（来自工具上下文）。
func TestSearchDefaultCurrentSession(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewSearchTool(workspace)

	result := tool.Execute(sessionContext("agent:main:demo"), map[string]any{"query": "bug-123"})
	if result.IsError {
		t.Fatalf("session_search 返回错误: %s", resultContent(result))
	}
	text := resultContent(result)
	if !strings.Contains(text, "session=agent:main:demo") {
		t.Fatalf("缺省会话未命中当前会话:\n%s", text)
	}
	if strings.Contains(text, "session=agent:main:other") || strings.Contains(text, "agent:main:skip") {
		t.Fatalf("缺省会话检索越过了当前会话:\n%s", text)
	}
}

// TestSearchCaseSensitivity 覆盖 case_sensitive 默认关闭与显式开启。
func TestSearchCaseSensitivity(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewSearchTool(workspace)

	insensitive := tool.Execute(context.Background(), map[string]any{
		"query": "BUG-123", "session": "*",
	})
	if insensitive.IsError || !strings.Contains(resultContent(insensitive), "hits=") {
		t.Fatalf("默认不区分大小写检索失败: %s", resultContent(insensitive))
	}
	if strings.Contains(resultContent(insensitive), "未找到匹配") {
		t.Fatalf("默认不区分大小写应命中 BUG-123:\n%s", resultContent(insensitive))
	}

	sensitive := tool.Execute(context.Background(), map[string]any{
		"query": "BUG-123", "session": "*", "case_sensitive": true,
	})
	if sensitive.IsError {
		t.Fatalf("区分大小写检索返回错误: %s", resultContent(sensitive))
	}
	if !strings.Contains(resultContent(sensitive), "未找到匹配") {
		t.Fatalf("区分大小写时不应命中小写 bug-123:\n%s", resultContent(sensitive))
	}
}

// TestSearchRegex 覆盖 regex 模式与非法正则诊断。
func TestSearchRegex(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewSearchTool(workspace)

	result := tool.Execute(context.Background(), map[string]any{
		"query": `bug-\d+`, "session": "*", "regex": true,
	})
	if result.IsError {
		t.Fatalf("正则检索返回错误: %s", resultContent(result))
	}
	if text := resultContent(result); !strings.Contains(text, "mode=regex") ||
		strings.Contains(text, "未找到匹配") {
		t.Fatalf("正则 bug-\\d+ 未命中:\n%s", text)
	}

	invalid := tool.Execute(context.Background(), map[string]any{
		"query": `(`, "regex": true,
	})
	if !invalid.IsError || !strings.Contains(resultContent(invalid), "正则表达式无效") {
		t.Fatalf("非法正则未被诊断: isError=%v content=%s", invalid.IsError, resultContent(invalid))
	}
}

// TestSearchLimitAndClamp 覆盖 limit 截断、默认值与硬上限钳制。
func TestSearchLimitAndClamp(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	capped := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "*", "limit": 1,
	})
	if capped.IsError {
		t.Fatalf("limit=1 检索返回错误: %s", resultContent(capped))
	}
	text := resultContent(capped)
	if got := strings.Count(text, "\n- session="); got != 1 {
		t.Fatalf("limit=1 返回 %d 条命中, want 1:\n%s", got, text)
	}
	if !strings.Contains(text, "结果数已达到 limit=1 上限") {
		t.Fatalf("limit 截断缺少提示:\n%s", text)
	}

	clampedTool := NewSearchToolWithOptions(workspace, Options{MaxLimit: 2, DefaultLimit: 1})
	clamped := clampedTool.Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "*", "limit": 999,
	})
	clampedText := resultContent(clamped)
	if got := strings.Count(clampedText, "\n- session="); got > 2 {
		t.Fatalf("limit=999 未被 MaxLimit=2 钳制: %d 条\n%s", got, clampedText)
	}
	if !strings.Contains(clampedText, "limit=2") {
		t.Fatalf("钳制后结果头未显示 limit=2:\n%s", clampedText)
	}

	defaulted := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "*", "limit": 0,
	})
	if strings.Contains(resultContent(defaulted), "limit=0") {
		t.Fatalf("limit=0 应回落默认值:\n%s", resultContent(defaulted))
	}
}

// TestSearchCorruptAndOversizeLines 覆盖损坏行与超长行的跳过与诊断。
func TestSearchCorruptAndOversizeLines(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	corrupt := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "内容", "session": "agent:main:corrupt",
	})
	if corrupt.IsError {
		t.Fatalf("含损坏行的检索不应失败: %s", resultContent(corrupt))
	}
	text := resultContent(corrupt)
	if !strings.Contains(text, "无法解析的 JSONL") {
		t.Fatalf("损坏行缺少诊断:\n%s", text)
	}
	if !strings.Contains(text, "损坏行之后的内容") {
		t.Fatalf("损坏行之后的有效内容未被检索:\n%s", text)
	}

	oversize := NewSearchToolWithOptions(workspace, Options{MaxLineBytes: 256}).
		Execute(context.Background(), map[string]any{
			"query": "end-marker", "session": "agent:main:long",
		})
	if oversize.IsError {
		t.Fatalf("超长行扫描不应失败: %s", resultContent(oversize))
	}
	oversizeText := resultContent(oversize)
	if !strings.Contains(oversizeText, "超过单行字节上限") {
		t.Fatalf("超长行缺少诊断:\n%s", oversizeText)
	}
	if !strings.Contains(oversizeText, "未找到匹配") {
		t.Fatalf("被跳过的超长行不应命中:\n%s", oversizeText)
	}
}

// TestSearchSnippetTruncationAndMasking 覆盖片段截断与敏感信息遮罩。
func TestSearchSnippetTruncationAndMasking(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	secret := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "api_key", "session": "agent:main:demo",
	})
	if secret.IsError {
		t.Fatalf("遮罩检索返回错误: %s", resultContent(secret))
	}
	secretText := resultContent(secret)
	if strings.Contains(secretText, "sk-abcdefghijklmnop") {
		t.Fatalf("摘要片段泄露了疑似密钥:\n%s", secretText)
	}
	if !strings.Contains(secretText, "***") {
		t.Fatalf("摘要片段缺少遮罩标记:\n%s", secretText)
	}

	truncated := NewSearchToolWithOptions(workspace, Options{MaxSnippetRunes: 30}).
		Execute(context.Background(), map[string]any{
			"query": "TRUNCATE-ME", "session": "agent:main:snippets",
		})
	if truncated.IsError {
		t.Fatalf("截断检索返回错误: %s", resultContent(truncated))
	}
	text := resultContent(truncated)
	if !strings.Contains(text, "TRUNCATE-ME") {
		t.Fatalf("截断片段丢失命中词:\n%s", text)
	}
	var snippet string
	for _, line := range strings.Split(text, "\n") {
		if idx := strings.Index(line, " user: "); idx >= 0 {
			snippet = line[idx+len(" user: "):]
		}
	}
	if snippet == "" {
		t.Fatalf("未解析到片段行:\n%s", text)
	}
	if !strings.HasSuffix(snippet, "…") {
		t.Fatalf("超长片段未以省略号截断: %q", snippet)
	}
	if utf8.RuneCountInString(snippet) > 31 {
		t.Fatalf("片段 rune 数 = %d, want <= 31 (30 + …)", utf8.RuneCountInString(snippet))
	}
}

// TestSearchSymlinkAndOutsideRootIgnored 覆盖只读 sessions 目录、不跟随符号链接。
func TestSearchSymlinkAndOutsideRootIgnored(t *testing.T) {
	workspace, sessionsDir := buildMainFixture(t)
	if _, err := os.Lstat(filepath.Join(sessionsDir, "agent_main_link.jsonl")); err != nil {
		t.Skip("当前平台不支持符号链接，跳过该断言")
	}
	tool := NewSearchTool(workspace)

	for _, query := range []string{"outside-secret", "outside-root-marker"} {
		result := tool.Execute(context.Background(), map[string]any{
			"query": query, "session": "*",
		})
		if result.IsError {
			t.Fatalf("query=%q 检索返回错误: %s", query, resultContent(result))
		}
		if !strings.Contains(resultContent(result), "未找到匹配") {
			t.Fatalf("query=%q 命中了 sessions 目录外/符号链接内容:\n%s", query, resultContent(result))
		}
	}
}

// TestSearchMetaSkipHonored 覆盖 meta.skip 逻辑截断语义。
func TestSearchMetaSkipHonored(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	result := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "agent:main:skip",
	})
	if result.IsError {
		t.Fatalf("检索返回错误: %s", resultContent(result))
	}
	text := resultContent(result)
	if !strings.Contains(text, "sessions/agent_main_skip.jsonl:2") {
		t.Fatalf("skip 之后第 2 行未命中:\n%s", text)
	}
	if strings.Contains(text, "sessions/agent_main_skip.jsonl:1") {
		t.Fatalf("skip 之前的历史不应被检索:\n%s", text)
	}
}

// TestSearchUnknownSessionAndInvalidArgs 覆盖未知会话与非法参数诊断。
func TestSearchUnknownSessionAndInvalidArgs(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewSearchTool(workspace)

	unknown := tool.Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "no:such:session",
	})
	if !unknown.IsError || !strings.Contains(resultContent(unknown), "未找到会话") {
		t.Fatalf("未知会话未被诊断: isError=%v content=%s", unknown.IsError, resultContent(unknown))
	}

	traversal := tool.Execute(context.Background(), map[string]any{
		"query": "root", "session": "../../etc/passwd",
	})
	if !traversal.IsError || !strings.Contains(resultContent(traversal), "未找到会话") {
		t.Fatalf("路径穿越式 session 未被拒绝: isError=%v content=%s", traversal.IsError, resultContent(traversal))
	}

	for name, args := range map[string]map[string]any{
		"missing query": {},
		"blank query":   {"query": "   "},
		"nul query":     {"query": "a\x00b"},
		"wrong limit":   {"query": "x", "limit": "5"},
		"wrong regex":   {"query": "x", "regex": "yes"},
		"wrong session": {"query": "x", "session": 42},
	} {
		result := tool.Execute(context.Background(), args)
		if result == nil || !result.IsError {
			t.Errorf("%s: 期望可诊断错误结果, got %+v", name, result)
			continue
		}
		if !strings.Contains(resultContent(result), "session_search 失败") {
			t.Errorf("%s: 错误结果缺少工具名前缀: %s", name, resultContent(result))
		}
	}
}

// TestSearchFileAndByteLimits 覆盖单次扫描文件数/字节上限。
func TestSearchFileAndByteLimits(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	fileLimited := NewSearchToolWithOptions(workspace, Options{MaxFiles: 1}).
		Execute(context.Background(), map[string]any{
			"query": "FIXME", "session": "*",
		})
	if fileLimited.IsError {
		t.Fatalf("文件数上限检索返回错误: %s", resultContent(fileLimited))
	}
	fileLimitedText := resultContent(fileLimited)
	if !strings.Contains(fileLimitedText, "未找到匹配") {
		t.Fatalf("MaxFiles=1 时不应扫描到 FIXME 所在文件:\n%s", fileLimitedText)
	}
	if !strings.Contains(fileLimitedText, "文件数上限") {
		t.Fatalf("文件数达到上限缺少诊断:\n%s", fileLimitedText)
	}

	byteLimited := NewSearchToolWithOptions(workspace, Options{MaxTotalBytes: 10}).
		Execute(context.Background(), map[string]any{
			"query": "bug-123", "session": "*",
		})
	if byteLimited.IsError {
		t.Fatalf("字节上限检索返回错误: %s", resultContent(byteLimited))
	}
	if !strings.Contains(resultContent(byteLimited), "字节上限") {
		t.Fatalf("字节达到上限缺少诊断:\n%s", resultContent(byteLimited))
	}
}

// TestSearchTimeout 覆盖协作式扫描超时（确定性：总超时 1ns 必然已过期）。
func TestSearchTimeout(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	result := NewSearchToolWithOptions(workspace, Options{Timeout: time.Nanosecond}).
		Execute(context.Background(), map[string]any{
			"query": "bug-123", "session": "*",
		})
	if !result.IsError {
		t.Fatalf("超时应返回可诊断错误, got %s", resultContent(result))
	}
	if !strings.Contains(resultContent(result), "扫描超时") {
		t.Fatalf("超时错误缺少诊断: %s", resultContent(result))
	}
}

// TestSearchEdgeEncodings 覆盖无末尾换行的文件与 Unicode/大小写折叠片段安全。
func TestSearchEdgeEncodings(t *testing.T) {
	workspace, sessionsDir := newTestWorkspace(t)

	noNewline := []byte(`{"role":"user","content":"末尾无换行 BUG-123"}`)
	if err := os.WriteFile(filepath.Join(sessionsDir, "agent_main_nonewline.jsonl"), noNewline, 0o644); err != nil {
		t.Fatalf("写入无换行 JSONL 失败: %v", err)
	}
	unicodeLine := []byte(`{"role":"user","content":"ÄÖÜ 与 Kelvin K 以及 bug-123 结尾"}` + "\n")
	if err := os.WriteFile(filepath.Join(sessionsDir, "agent_main_unicode.jsonl"), unicodeLine, 0o644); err != nil {
		t.Fatalf("写入 Unicode JSONL 失败: %v", err)
	}

	tool := NewSearchTool(workspace)
	noNewlineResult := tool.Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "agent_main_nonewline",
	})
	if noNewlineResult.IsError {
		t.Fatalf("无末尾换行文件检索失败: %s", resultContent(noNewlineResult))
	}
	if !strings.Contains(resultContent(noNewlineResult), "末尾无换行 BUG-123") {
		t.Fatalf("无末尾换行的最后一行未命中:\n%s", resultContent(noNewlineResult))
	}

	unicodeResult := tool.Execute(context.Background(), map[string]any{
		"query": "äöü", "session": "agent_main_unicode",
	})
	if unicodeResult.IsError {
		t.Fatalf("Unicode 大小写折叠检索失败: %s", resultContent(unicodeResult))
	}
	if !strings.Contains(resultContent(unicodeResult), "ÄÖÜ") {
		t.Fatalf("Unicode 大小写折叠未命中:\n%s", resultContent(unicodeResult))
	}
}

// TestSearchMissingSessionsDir 覆盖 workspace 没有 sessions 目录：明确无匹配而非错误。
func TestSearchMissingSessionsDir(t *testing.T) {
	result := NewSearchTool(t.TempDir()).Execute(context.Background(), map[string]any{"query": "x"})
	if result.IsError {
		t.Fatalf("空 workspace 检索不应报错: %s", resultContent(result))
	}
	if !strings.Contains(resultContent(result), "未找到匹配") {
		t.Fatalf("空 workspace 结果缺少明确说明: %s", resultContent(result))
	}
}

// TestSearchBadMetaFallsBackToStem 覆盖 meta 无法解析时按文件名 stem 识别并诊断。
func TestSearchBadMetaFallsBackToStem(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	result := NewSearchTool(workspace).Execute(context.Background(), map[string]any{
		"query": "bug-123", "session": "*",
	})
	text := resultContent(result)
	if !strings.Contains(text, "session=agent_main_badmeta") {
		t.Fatalf("badmeta 会话未按文件名 stem 识别:\n%s", text)
	}
	if !strings.Contains(text, "meta 无法解析") {
		t.Fatalf("badmeta 缺少诊断:\n%s", text)
	}
}
