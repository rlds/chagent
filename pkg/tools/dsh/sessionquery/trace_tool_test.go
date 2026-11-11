package sessionquery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/providers"
)

// TestTraceSessionSummary 覆盖单会话时间线摘要：消息/回合/工具调用计数、起止时间、meta 摘要与回合行号范围。
func TestTraceSessionSummary(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	result := NewTraceTool(workspace).Execute(context.Background(), map[string]any{
		"session": "agent:main:demo",
	})
	if result.IsError {
		t.Fatalf("session_trace 返回错误: %s", resultContent(result))
	}
	text := resultContent(result)

	for _, want := range []string{
		"session=agent:main:demo",
		"path=sessions/agent_main_demo.jsonl",
		// 7 条消息、2 个回合、1 次工具调用。
		"messages=7",
		"turns=2",
		"tool_calls=1",
		// 起止时间来自消息 created_at（固定夹具 03:04:05 起）。
		"start=2026-01-02T03:04:05Z",
		"end=2026-01-02T03:10:05Z",
		// 角色计数：user=2（真实用户）+1（上下文注入）？上下文也是 user 角色，故 user=3。
		"roles=assistant=3,tool=1,user=3",
		// meta 摘要。
		`summary="修复 bug-123 的会话"`,
		// 回合 1：第 2 行（真实用户）到第 5 行（最终回答）。
		"turn 1: lines 2-5 messages=4 tool_calls=1 roles=assistant=2,tool=1,user=1",
		// 回合 2：第 6-7 行。
		"turn 2: lines 6-7 messages=2 tool_calls=0 roles=assistant=1,user=1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("trace 输出缺少 %q:\n%s", want, text)
		}
	}
}

// TestTraceTurnFilter 覆盖 turn=N 过滤与不存在回合的明确说明。
func TestTraceTurnFilter(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewTraceTool(workspace)

	filtered := tool.Execute(context.Background(), map[string]any{
		"session": "agent:main:demo", "turn": 2,
	})
	if filtered.IsError {
		t.Fatalf("turn 过滤返回错误: %s", resultContent(filtered))
	}
	text := resultContent(filtered)
	if !strings.Contains(text, "turn 2: lines 6-7") {
		t.Fatalf("turn=2 未输出目标回合:\n%s", text)
	}
	if strings.Contains(text, "turn 1:") {
		t.Fatalf("turn=2 仍输出了其他回合:\n%s", text)
	}

	missing := tool.Execute(context.Background(), map[string]any{
		"session": "agent:main:demo", "turn": 9,
	})
	if missing.IsError {
		t.Fatalf("不存在回合不应返回错误: %s", resultContent(missing))
	}
	if !strings.Contains(resultContent(missing), "回合 9 不存在（该会话共 2 个回合）") {
		t.Fatalf("不存在回合缺少明确说明:\n%s", resultContent(missing))
	}
}

// TestTraceAllSessionsAndLimit 覆盖 session=* 与 limit 截断。
func TestTraceAllSessionsAndLimit(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	all := NewTraceTool(workspace).Execute(context.Background(), map[string]any{"session": "*"})
	if all.IsError {
		t.Fatalf("全量 trace 返回错误: %s", resultContent(all))
	}
	allText := resultContent(all)
	if strings.Count(allText, "\n- session=") < 3 {
		t.Fatalf("session=* 结果会话数过少:\n%s", allText)
	}

	limited := NewTraceTool(workspace).Execute(context.Background(), map[string]any{
		"session": "*", "limit": 2,
	})
	if limited.IsError {
		t.Fatalf("limit trace 返回错误: %s", resultContent(limited))
	}
	limitedText := resultContent(limited)
	if got := strings.Count(limitedText, "\n- session="); got != 2 {
		t.Fatalf("limit=2 返回 %d 个会话, want 2:\n%s", got, limitedText)
	}
	if !strings.Contains(limitedText, "会话数已达到 limit=2 上限") {
		t.Fatalf("会话数截断缺少提示:\n%s", limitedText)
	}
}

// TestTraceDefaultCurrentSession 覆盖缺省 session=当前会话。
func TestTraceDefaultCurrentSession(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	result := NewTraceTool(workspace).Execute(sessionContext("agent:main:demo"), map[string]any{})
	if result.IsError {
		t.Fatalf("缺省会话 trace 返回错误: %s", resultContent(result))
	}
	text := resultContent(result)
	if !strings.Contains(text, "session=agent:main:demo") {
		t.Fatalf("缺省会话未定位到当前会话:\n%s", text)
	}
	if strings.Count(text, "\n- session=") != 1 {
		t.Fatalf("缺省会话 trace 返回了多个会话:\n%s", text)
	}
}

// TestTraceCorruptLinesDiagnostics 覆盖损坏行/超长行的计数与诊断。
func TestTraceCorruptLinesDiagnostics(t *testing.T) {
	workspace, _ := buildMainFixture(t)

	corrupt := NewTraceTool(workspace).Execute(context.Background(), map[string]any{
		"session": "agent:main:corrupt",
	})
	if corrupt.IsError {
		t.Fatalf("含损坏行的 trace 不应失败: %s", resultContent(corrupt))
	}
	text := resultContent(corrupt)
	// 3 行中 1 行损坏 → 2 条消息；没有回合边界（assistant 开头），turns=0。
	if !strings.Contains(text, "messages=2") {
		t.Fatalf("损坏行未被排除在消息计数之外:\n%s", text)
	}
	if !strings.Contains(text, "无法解析的 JSONL") {
		t.Fatalf("损坏行缺少诊断:\n%s", text)
	}

	oversize := NewTraceToolWithOptions(workspace, Options{MaxLineBytes: 256}).
		Execute(context.Background(), map[string]any{"session": "agent:main:long"})
	if oversize.IsError {
		t.Fatalf("超长行 trace 不应失败: %s", resultContent(oversize))
	}
	if !strings.Contains(resultContent(oversize), "超过单行字节上限") {
		t.Fatalf("超长行缺少诊断:\n%s", resultContent(oversize))
	}
}

// TestTraceUnknownSessionAndInvalidArgs 覆盖未知会话与非法参数。
func TestTraceUnknownSessionAndInvalidArgs(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	tool := NewTraceTool(workspace)

	unknown := tool.Execute(context.Background(), map[string]any{"session": "no:such"})
	if !unknown.IsError || !strings.Contains(resultContent(unknown), "未找到会话") {
		t.Fatalf("未知会话未被诊断: isError=%v content=%s", unknown.IsError, resultContent(unknown))
	}

	negativeTurn := tool.Execute(context.Background(), map[string]any{"turn": -1})
	if !negativeTurn.IsError || !strings.Contains(resultContent(negativeTurn), "turn") {
		t.Fatalf("负 turn 未被诊断: isError=%v content=%s", negativeTurn.IsError, resultContent(negativeTurn))
	}

	wrongType := tool.Execute(context.Background(), map[string]any{"turn": "2"})
	if !wrongType.IsError {
		t.Fatalf("字符串 turn 应返回可诊断错误: %s", resultContent(wrongType))
	}
}

// TestTraceTimeoutAndMissingDir 覆盖超时与 sessions 目录缺失。
func TestTraceTimeoutAndMissingDir(t *testing.T) {
	workspace, _ := buildMainFixture(t)
	timeout := NewTraceToolWithOptions(workspace, Options{Timeout: time.Nanosecond}).
		Execute(context.Background(), map[string]any{"session": "*"})
	if !timeout.IsError || !strings.Contains(resultContent(timeout), "扫描超时") {
		t.Fatalf("超时未被诊断: isError=%v content=%s", timeout.IsError, resultContent(timeout))
	}

	empty := NewTraceTool(t.TempDir()).Execute(context.Background(), map[string]any{})
	if empty.IsError {
		t.Fatalf("空 workspace trace 不应报错: %s", resultContent(empty))
	}
	if !strings.Contains(resultContent(empty), "未找到可摘要的会话") {
		t.Fatalf("空 workspace 缺少明确说明:\n%s", resultContent(empty))
	}
}

// TestTraceSummaryMasked 覆盖 meta 摘要中的敏感信息遮罩。
func TestTraceSummaryMasked(t *testing.T) {
	workspace, sessionsDir := newTestWorkspace(t)
	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem:    "agent_main_secret",
		Key:     "agent:main:secret",
		Summary: "会话摘要 api_key=sk-supersecretvalue",
		Messages: []providers.Message{
			userMessage("hi", fixtureTime(0)),
		},
	})
	result := NewTraceTool(workspace).Execute(context.Background(), map[string]any{
		"session": "agent:main:secret",
	})
	if result.IsError {
		t.Fatalf("trace 返回错误: %s", resultContent(result))
	}
	text := resultContent(result)
	if strings.Contains(text, "sk-supersecretvalue") {
		t.Fatalf("meta 摘要泄露疑似密钥:\n%s", text)
	}
	if !strings.Contains(text, "***") {
		t.Fatalf("meta 摘要缺少遮罩标记:\n%s", text)
	}
}
