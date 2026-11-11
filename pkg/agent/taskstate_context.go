package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/taskstate"
)

// TaskStateContextKind 是 P-09 任务状态块的持久上下文类型。
// agent 循环会把它附加到注入的上下文消息上，以便压缩与去重时能够识别它。
const TaskStateContextKind = "task-state"

// TaskState 渲染限制。这些限制可避免在长时间运行的任务中，状态块挤占
// 工作区上下文与工具结果的空间。
const (
	DefaultTaskStateMaxObjectiveRunes = 500
	DefaultTaskStateMaxTodoRunes      = 200
	DefaultTaskStateMaxEvidence       = 5
	DefaultTaskStateMaxEvidenceRunes  = 500
	DefaultTaskStateMaxBlockedRunes   = 500
	DefaultTaskStateTruncationSuffix  = "…"
)

// TaskStateRenderOptions 控制渲染后的 P-09 块的截断行为。
// 零值会回退到 Default* 系列常量。
type TaskStateRenderOptions struct {
	MaxObjectiveRunes int
	MaxTodoRunes      int
	MaxEvidence       int
	MaxEvidenceRunes  int
	MaxBlockedRunes   int
}

// DefaultTaskStateRenderOptions 返回生产环境的渲染限制。
func DefaultTaskStateRenderOptions() TaskStateRenderOptions {
	return TaskStateRenderOptions{
		MaxObjectiveRunes: DefaultTaskStateMaxObjectiveRunes,
		MaxTodoRunes:      DefaultTaskStateMaxTodoRunes,
		MaxEvidence:       DefaultTaskStateMaxEvidence,
		MaxEvidenceRunes:  DefaultTaskStateMaxEvidenceRunes,
		MaxBlockedRunes:   DefaultTaskStateMaxBlockedRunes,
	}
}

func (o TaskStateRenderOptions) withDefaults() TaskStateRenderOptions {
	if o.MaxObjectiveRunes <= 0 {
		o.MaxObjectiveRunes = DefaultTaskStateMaxObjectiveRunes
	}
	if o.MaxTodoRunes <= 0 {
		o.MaxTodoRunes = DefaultTaskStateMaxTodoRunes
	}
	if o.MaxEvidence <= 0 {
		o.MaxEvidence = DefaultTaskStateMaxEvidence
	}
	if o.MaxEvidenceRunes <= 0 {
		o.MaxEvidenceRunes = DefaultTaskStateMaxEvidenceRunes
	}
	if o.MaxBlockedRunes <= 0 {
		o.MaxBlockedRunes = DefaultTaskStateMaxBlockedRunes
	}
	return o
}

// RenderTaskState 使用默认渲染限制，以中文渲染 P-09 `task_state` 块。
// 零值状态（没有任务的普通聊天）会渲染为空字符串，这样调用方就不会注入任何内容。
func RenderTaskState(state taskstate.State) string {
	return RenderTaskStateWithOptions(state, DefaultTaskStateRenderOptions())
}

// RenderTaskStateWithOptions 以中文渲染 P-09 `task_state` 块。
//
// 阶段值、字段名与段落标签遵循提示词规范：
//
//	<task_state>
//	目标："..."
//	阶段：active
//	轮次：2/20
//	待办：
//	- [completed] ...
//	最近检查点：2026-10-28T16:10:00+08:00
//	证据：
//	- ...
//	阻塞原因：无
//	</task_state>
//
// 所有用户可控文本都会针对闭合标签进行转义，以免模型跳出该块。
// 目标/待办/证据文本按 rune 边界截断。
func RenderTaskStateWithOptions(state taskstate.State, opts TaskStateRenderOptions) string {
	if state.IsZero() {
		return ""
	}
	opts = opts.withDefaults()

	var b strings.Builder
	b.WriteString("<task_state>\n")
	fmt.Fprintf(&b, "目标：%q\n", truncateTaskStateText(state.Objective, opts.MaxObjectiveRunes))
	fmt.Fprintf(&b, "阶段：%s\n", escapeTaskStateText(string(state.Phase)))
	fmt.Fprintf(&b, "轮次：%d/%d\n", state.Round, state.MaxRounds)

	if len(state.Todos) == 0 {
		b.WriteString("待办：无\n")
	} else {
		b.WriteString("待办：\n")
		for _, todo := range state.Todos {
			fmt.Fprintf(
				&b,
				"- [%s] %s\n",
				escapeTaskStateText(todo.Status),
				truncateTaskStateText(todo.Content, opts.MaxTodoRunes),
			)
		}
	}

	if state.LastProgressAt.IsZero() {
		b.WriteString("最近检查点：无\n")
	} else {
		fmt.Fprintf(&b, "最近检查点：%s\n", state.LastProgressAt.Format(time.RFC3339))
	}

	evidence := state.Evidence
	if len(evidence) == 0 {
		b.WriteString("证据：无\n")
	} else {
		if len(evidence) > opts.MaxEvidence {
			evidence = evidence[len(evidence)-opts.MaxEvidence:]
		}
		b.WriteString("证据：\n")
		for _, item := range evidence {
			fmt.Fprintf(&b, "- %s\n", truncateTaskStateText(item, opts.MaxEvidenceRunes))
		}
	}

	if state.Phase == taskstate.PhaseBlocked && state.BlockedReason != nil {
		fmt.Fprintf(
			&b,
			"阻塞原因：%s：%s\n",
			escapeTaskStateText(state.BlockedReason.Code),
			truncateTaskStateText(state.BlockedReason.Message, opts.MaxBlockedRunes),
		)
	} else {
		b.WriteString("阻塞原因：无\n")
	}
	b.WriteString("</task_state>")
	return b.String()
}

var taskStateClosingTagRe = regexp.MustCompile(`(?i)</task_state>`)

// escapeTaskStateText 防止用户可控文本提前闭合 `task_state` 块。
func escapeTaskStateText(text string) string {
	if !strings.Contains(strings.ToLower(text), "</task_state>") {
		return text
	}
	return taskStateClosingTagRe.ReplaceAllString(text, `<\/task_state>`)
}

// truncateTaskStateText 将文本规范化为单行，并按 UTF-8 rune 边界截断。
func truncateTaskStateText(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return escapeTaskStateText(text)
	}
	runes := []rune(text)
	truncated := string(runes[:maxRunes]) + DefaultTaskStateTruncationSuffix
	return escapeTaskStateText(truncated)
}

// TaskStateDigest 返回任务状态中必须触发新持久上下文注入的字段的稳定摘要：
// 目标、阶段、最大轮次、待办、证据和阻塞原因。
//
// 轮次、时间戳与进度计数器被有意排除：轮次推进由 P-10 task_round 提示词承载，
// 若每轮都重新注入整个状态会使上下文膨胀。空状态摘要为 ""。
func TaskStateDigest(state taskstate.State) string {
	if state.IsZero() {
		return ""
	}
	normalized := struct {
		Objective     string                   `json:"objective"`
		Phase         string                   `json:"phase"`
		MaxRounds     int                      `json:"max_rounds"`
		Todos         []taskstate.Todo         `json:"todos"`
		Evidence      []string                 `json:"evidence"`
		BlockedReason *taskstate.BlockedReason `json:"blocked_reason"`
	}{
		Objective:     strings.Join(strings.Fields(strings.TrimSpace(state.Objective)), " "),
		Phase:         string(state.Phase),
		MaxRounds:     state.MaxRounds,
		Todos:         state.Todos,
		Evidence:      state.Evidence,
		BlockedReason: state.BlockedReason,
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		// 该结构体只包含字符串/整数/切片，因此这里不可达；
		// 退回基于内容的值以保持结果确定。
		return fmt.Sprintf("%s|%s|%d", normalized.Objective, normalized.Phase, normalized.MaxRounds)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TaskStateChanged 报告相对于此前注入的状态，下一个状态是否应作为新的
// 持久上下文块注入。
//
//   - next 没有任务（零值状态）：绝不注入；
//   - 没有先前状态：注入首个状态；
//   - 其他情况：仅当 TaskStateDigest 发生变化时才注入。
func TaskStateChanged(previous *taskstate.State, next taskstate.State) bool {
	if next.IsZero() {
		return false
	}
	if previous == nil || previous.IsZero() {
		return true
	}
	return TaskStateDigest(*previous) != TaskStateDigest(next)
}
