// session_trace：按会话或回合输出会话历史的时间线摘要。
//
// 对应 DSH `packages/experimental/tool-session-query` 的 `session_trace` 工具。
// DSH 的 trace 是会话 lineage（祖先/后代会话）；chagent 没有 lineage 存储，
// 因此本工具把 trace 语义适配为“会话 → 回合”的时间线摘要：
// 消息数、角色计数、工具调用数、起止时间、meta 摘要，以及每个回合的
// 行号范围、消息数与工具调用数。

package sessionquery

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/tools/shared"
)

// TraceTool 实现 `session_trace`。
type TraceTool struct {
	workspace string
	opts      Options
}

// NewTraceTool 用默认上限构造 session_trace 工具。
func NewTraceTool(workspace string) *TraceTool {
	return NewTraceToolWithOptions(workspace, DefaultOptions())
}

// NewTraceToolWithOptions 用自定义上限构造 session_trace 工具（零值上限回落默认）。
func NewTraceToolWithOptions(workspace string, opts Options) *TraceTool {
	return &TraceTool{workspace: workspace, opts: opts.normalized()}
}

// Name 返回稳定工具名。
func (t *TraceTool) Name() string { return TraceToolName }

// Description 返回模型可见的中文工具描述。
func (t *TraceTool) Description() string {
	return "读取 workspace 会话历史的时间线摘要：消息数、工具调用数、起止时间、" +
		"meta 摘要，以及每个回合的行号范围与角色计数。只读且不跟随符号链接；" +
		"默认查看当前会话，session=\"*\" 查看全部会话，turn=N 只看某个回合。"
}

// PromptMetadata 返回 DSH 工具组的提示元数据。
func (t *TraceTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// ConcurrencySafe 声明该工具只读，可与其他只读工具并行执行。
func (t *TraceTool) ConcurrencySafe() bool { return true }

// Parameters 返回模型参数 schema（中文描述）。
func (t *TraceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session": map[string]any{
				"type": "string",
				"description": "可选：目标会话 key（也接受 sessions 文件名，可不含 .jsonl）；" +
					"缺省查看当前会话，\"*\" 查看全部会话",
			},
			"turn": map[string]any{
				"type":        "integer",
				"description": "可选：只输出该回合（从 1 开始计数；缺省输出全部回合）",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("可选：最多输出多少个会话的摘要，默认 %d，上限 %d", DefaultLimit, MaxLimit),
			},
		},
	}
}

// turnTrace 是一个回合的时间线摘要。
type turnTrace struct {
	Turn       int
	StartLine  int
	EndLine    int
	Messages   int
	ToolCalls  int
	RoleCounts map[string]int
	StartedAt  time.Time
	EndedAt    time.Time
}

// sessionTrace 是一个会话的时间线摘要。
type sessionTrace struct {
	Key        string
	RelPath    string
	Messages   int
	ToolCalls  int
	RoleCounts map[string]int
	Turns      []turnTrace
	StartedAt  time.Time
	EndedAt    time.Time
	Summary    string
}

// Execute 执行时间线摘要。
func (t *TraceTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	if t == nil {
		return toolshared.ErrorResult("session_trace 失败: 工具未初始化")
	}
	opts := t.opts

	sessionArg, _, err := stringArg(args, "session")
	if err != nil {
		return toolError(TraceToolName, err)
	}
	turnArg, turnPresent, err := intArg(args, "turn")
	if err != nil {
		return toolError(TraceToolName, err)
	}
	if turnPresent && turnArg < 0 {
		return toolshared.ErrorResult(`session_trace 失败: 参数 "turn" 必须是非负整数（0 或缺省表示全部回合）`)
	}
	limitArg, limitPresent, err := intArg(args, "limit")
	if err != nil {
		return toolError(TraceToolName, err)
	}
	limit, clamped, err := opts.resolveLimit(limitArg, limitPresent)
	if err != nil {
		return toolError(TraceToolName, err)
	}

	selector, scopeNote := resolveDefaultSessionSelector(ctx, sessionArg)
	discovery, listErr := listSessions(t.workspace)
	if listErr != nil {
		return toolError(TraceToolName, listErr)
	}
	sessions := filterSessions(discovery.Sessions, selector)
	explicitSelector := strings.TrimSpace(sessionArg) != "" && selector != "*"
	if explicitSelector && len(sessions) == 0 {
		return toolshared.ErrorResult(fmt.Sprintf(
			"session_trace 失败: 未找到会话 %q（已发现 %d 个会话文件；可用 session=\"*\" 查看全部）",
			selector, len(discovery.Sessions),
		))
	}

	scanCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	diags := scanDiagnostics{FilesSkipped: discovery.SkippedLinks}
	if len(discovery.BadMetaKeys) > 0 {
		diags.UnreadableFiles = append(diags.UnreadableFiles,
			"meta 无法解析（已按文件名识别，摘要不可用）: "+strings.Join(discovery.BadMetaKeys, ", "))
	}

	traces := make([]sessionTrace, 0, limit)
	scannedFiles := 0
	capped := false
	for _, sf := range sessions {
		if len(traces) >= limit {
			capped = true
			break
		}
		if scannedFiles >= opts.MaxFiles {
			diags.HitFileLimit = true
			break
		}
		scannedFiles++

		trace, scanErr := traceSessionFile(scanCtx, sf, opts, &diags)
		if scanErr != nil {
			continue
		}
		traces = append(traces, trace)
		if diags.HitTimeLimit || diags.HitByteLimit {
			break
		}
	}

	if diags.HitTimeLimit {
		return toolshared.ErrorResult(fmt.Sprintf(
			"session_trace 失败: 扫描超时（上限 %s），已摘要 %d 个会话但结果不完整；"+
				"请指定 session 或提高上限。",
			opts.Timeout, len(traces),
		))
	}

	return toolshared.SilentResult(renderTraceResult(selector, scopeNote, traces, turnArg, turnPresent, limit, capped, clamped, diags))
}

// traceSessionFile 扫描单个会话文件并计算时间线摘要。
func traceSessionFile(
	ctx context.Context,
	sf sessionFile,
	opts Options,
	diags *scanDiagnostics,
) (sessionTrace, error) {
	trace := sessionTrace{
		Key:        sf.Key,
		RelPath:    sf.RelPath,
		RoleCounts: map[string]int{},
		Summary:    snippetAround(sf.Meta.Summary, "", opts.MaxSnippetRunes),
	}
	turn := 0
	turnIndex := -1
	firstAt := time.Time{}
	lastAt := time.Time{}

	scanErr := scanSessionFile(ctx, sf, opts, diags, func(line scanLine) bool {
		msg := line.Msg
		at := time.Time{}
		if msg.CreatedAt != nil {
			at = *msg.CreatedAt
		}

		trace.Messages++
		trace.RoleCounts[roleLabel(msg.Role)]++
		toolCalls := len(msg.ToolCalls)
		trace.ToolCalls += toolCalls

		if isTurnBoundaryMessage(msg) {
			turn++
			turnIndex = len(trace.Turns)
			trace.Turns = append(trace.Turns, turnTrace{
				Turn:       turn,
				StartLine:  line.Line,
				EndLine:    line.Line,
				RoleCounts: map[string]int{},
			})
		}
		if turn >= 1 && turnIndex >= 0 && turnIndex < len(trace.Turns) {
			current := &trace.Turns[turnIndex]
			current.EndLine = line.Line
			current.Messages++
			current.RoleCounts[roleLabel(msg.Role)]++
			current.ToolCalls += toolCalls
			if !at.IsZero() {
				if current.StartedAt.IsZero() {
					current.StartedAt = at
				}
				current.EndedAt = at
			}
		}

		if !at.IsZero() {
			if firstAt.IsZero() {
				firstAt = at
			}
			lastAt = at
		}
		return true
	})
	if scanErr != nil {
		return sessionTrace{}, scanErr
	}

	// 时间兜底：消息没有 created_at 时用 meta 时间，最后用文件 mtime。
	trace.StartedAt = firstAt
	trace.EndedAt = lastAt
	if trace.StartedAt.IsZero() {
		trace.StartedAt = firstNonZeroTime(sf.Meta.CreatedAt, sf.Meta.UpdatedAt, sf.ModTime)
	}
	if trace.EndedAt.IsZero() {
		trace.EndedAt = firstNonZeroTime(sf.Meta.UpdatedAt, sf.Meta.CreatedAt, sf.ModTime)
	}
	for i := range trace.Turns {
		if trace.Turns[i].StartedAt.IsZero() {
			trace.Turns[i].StartedAt = trace.StartedAt
		}
		if trace.Turns[i].EndedAt.IsZero() {
			trace.Turns[i].EndedAt = trace.StartedAt
		}
	}
	return trace, nil
}

// firstNonZeroTime 返回第一个非零时间（都为零则返回零值）。
func firstNonZeroTime(candidates ...time.Time) time.Time {
	for _, candidate := range candidates {
		if !candidate.IsZero() {
			return candidate
		}
	}
	return time.Time{}
}

// renderTraceResult 渲染模型可读结果。
func renderTraceResult(
	selector, scopeNote string,
	traces []sessionTrace,
	turnArg int, turnPresent bool,
	limit int,
	capped, clamped bool,
	diags scanDiagnostics,
) string {
	lines := []string{fmt.Sprintf(
		"session_trace: sessions=%s traced=%d limit=%d",
		maskSensitiveText(selector), len(traces), limit,
	)}
	if len(traces) == 0 {
		lines = append(lines, "", "未找到可摘要的会话。可尝试用 session=\"*\" 查看全部会话。")
	}
	for _, trace := range traces {
		lines = append(lines, fmt.Sprintf(
			"- session=%s path=%s messages=%d turns=%d tool_calls=%d start=%s end=%s roles=%s summary=%q",
			maskSensitiveText(trace.Key), trace.RelPath, trace.Messages, len(trace.Turns),
			trace.ToolCalls, formatTime(trace.StartedAt), formatTime(trace.EndedAt),
			formatRoleCounts(trace.RoleCounts), maskSensitiveText(trace.Summary),
		))
		renderedTurns := 0
		for _, turn := range trace.Turns {
			if turnPresent && turnArg > 0 && turn.Turn != turnArg {
				continue
			}
			renderedTurns++
			lines = append(lines, fmt.Sprintf(
				"  turn %d: lines %d-%d messages=%d tool_calls=%d roles=%s start=%s end=%s",
				turn.Turn, turn.StartLine, turn.EndLine, turn.Messages, turn.ToolCalls,
				formatRoleCounts(turn.RoleCounts), formatTime(turn.StartedAt), formatTime(turn.EndedAt),
			))
		}
		if turnPresent && turnArg > 0 && renderedTurns == 0 {
			lines = append(lines, fmt.Sprintf(
				"  回合 %d 不存在（该会话共 %d 个回合）", turnArg, len(trace.Turns),
			))
		}
	}
	if capped || clamped {
		lines = append(lines, "", fmt.Sprintf("会话数已达到 limit=%d 上限，可能还有未摘要的会话。", limit))
	}
	if scopeNote != "" {
		lines = append(lines, "", "说明: "+scopeNote)
	}
	lines = appendDiagnostics(lines, diags)
	return strings.Join(lines, "\n")
}

// formatRoleCounts 按角色名排序渲染计数，例如 `assistant=2,tool=1,user=1`。
func formatRoleCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}
