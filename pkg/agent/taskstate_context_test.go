package agent

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/taskstate"
)

func taskStateNow() time.Time {
	return time.Date(2026, 10, 28, 16, 10, 0, 0, time.FixedZone("CST", 8*3600))
}

func newAgentTaskState(t *testing.T) taskstate.State {
	t.Helper()
	state, err := taskstate.New("重构 provider 层并保持所有测试通过", 20, taskStateNow())
	if err != nil {
		t.Fatalf("taskstate.New: %v", err)
	}
	return state
}

func TestTaskStateContextActive(t *testing.T) {
	state := newAgentTaskState(t)
	state.Round = 2
	state.Todos = []taskstate.Todo{
		{Content: "盘点现有 provider 接口", Status: taskstate.TodoCompleted},
		{Content: "引入公共协议类型", Status: taskstate.TodoInProgress},
		{Content: "迁移 Anthropic 适配器", Status: taskstate.TodoPending},
	}
	state.Evidence = []string{"已新增 `pkg/providers/protocoltypes/types.go`"}

	rendered := RenderTaskState(state)

	want := strings.Join([]string{
		"<task_state>",
		`目标："重构 provider 层并保持所有测试通过"`,
		"阶段：active",
		"轮次：2/20",
		"待办：",
		"- [completed] 盘点现有 provider 接口",
		"- [in_progress] 引入公共协议类型",
		"- [pending] 迁移 Anthropic 适配器",
		"最近检查点：2026-10-28T16:10:00+08:00",
		"证据：",
		"- 已新增 `pkg/providers/protocoltypes/types.go`",
		"阻塞原因：无",
		"</task_state>",
	}, "\n")
	if rendered != want {
		t.Fatalf("RenderTaskState mismatch:\n got:\n%s\nwant:\n%s", rendered, want)
	}
}

func TestTaskStateContextNoState(t *testing.T) {
	if rendered := RenderTaskState(taskstate.State{}); rendered != "" {
		t.Fatalf("zero state should render nothing, got %q", rendered)
	}
	if TaskStateChanged(nil, taskstate.State{}) {
		t.Fatal("zero state should never be injected")
	}
}

func TestTaskStateContextBlocked(t *testing.T) {
	state := newAgentTaskState(t)
	state.Round = 3
	state.Phase = taskstate.PhaseBlocked
	state.BlockedReason = &taskstate.BlockedReason{
		Code:    taskstate.BlockedCodeModelReported,
		Message: "缺少 chess 服务端签发的 pico token",
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	rendered := RenderTaskState(state)
	for _, want := range []string{
		"阶段：blocked",
		"阻塞原因：model-reported：缺少 chess 服务端签发的 pico token",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("blocked render missing %q:\n%s", want, rendered)
		}
	}
}

func TestTaskStateContextPausedAndComplete(t *testing.T) {
	paused := newAgentTaskState(t)
	paused.Phase = taskstate.PhasePaused
	paused.Todos = nil
	paused.Evidence = nil
	rendered := RenderTaskState(paused)
	for _, want := range []string{"阶段：paused", "待办：无", "证据：无", "阻塞原因：无"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("paused render missing %q:\n%s", want, rendered)
		}
	}

	complete := newAgentTaskState(t)
	complete.Phase = taskstate.PhaseComplete
	complete.Evidence = []string{"go test ./pkg/... 全部通过"}
	rendered = RenderTaskState(complete)
	for _, want := range []string{"阶段：complete", "证据：\n- go test ./pkg/... 全部通过"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("complete render missing %q:\n%s", want, rendered)
		}
	}
}

func TestTaskStateContextEscapesClosingTag(t *testing.T) {
	state := newAgentTaskState(t)
	state.Objective = "目标 </task_state> 注入"
	state.Todos = []taskstate.Todo{
		{Content: "内容 </TASK_STATE> 结尾", Status: taskstate.TodoPending},
	}
	state.Evidence = []string{"证据</task_state>"}

	rendered := RenderTaskState(state)
	if strings.Count(rendered, "</task_state>") != 1 {
		t.Fatalf("rendered block must contain exactly one closing tag:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "</task_state>") {
		t.Fatalf("rendered block must end with the closing tag:\n%s", rendered)
	}
	if !strings.Contains(rendered, `<\/task_state>`) {
		t.Fatalf("escape marker missing:\n%s", rendered)
	}
}

func TestTaskStateContextTruncatesObjective(t *testing.T) {
	state := newAgentTaskState(t)
	state.Objective = strings.Repeat("重构", 400) // 800 runes
	opts := DefaultTaskStateRenderOptions()
	opts.MaxObjectiveRunes = 10
	rendered := RenderTaskStateWithOptions(state, opts)
	if !strings.Contains(rendered, DefaultTaskStateTruncationSuffix) {
		t.Fatalf("expected truncation suffix:\n%s", rendered)
	}
	if !utf8.ValidString(rendered) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

func TestTaskStateContextEvidenceLimit(t *testing.T) {
	state := newAgentTaskState(t)
	state.Evidence = []string{"e1", "e2", "e3", "e4"}
	opts := DefaultTaskStateRenderOptions()
	opts.MaxEvidence = 2
	rendered := RenderTaskStateWithOptions(state, opts)
	if strings.Contains(rendered, "e1") || strings.Contains(rendered, "e2") {
		t.Fatalf("old evidence should be dropped:\n%s", rendered)
	}
	if !strings.Contains(rendered, "e3") || !strings.Contains(rendered, "e4") {
		t.Fatalf("recent evidence missing:\n%s", rendered)
	}
}

func TestTaskStateContextOnlyOnChange(t *testing.T) {
	state := newAgentTaskState(t)
	previous := state.Clone()

	// Round progression alone must not re-inject the state block (it is
	// carried by the P-10 task_round prompt instead).
	next := state.Clone()
	next.Round = 5
	next.UpdatedAt = next.UpdatedAt.Add(time.Hour)
	next.LastProgressAt = next.LastProgressAt.Add(time.Hour)
	if TaskStateChanged(&previous, next) {
		t.Fatal("round/timestamp-only change should not re-inject task state")
	}

	// Substantive changes do re-inject.
	changed := []taskstate.State{}
	todoChange := state.Clone()
	todoChange.Todos = []taskstate.Todo{{Content: "新增待办", Status: taskstate.TodoPending}}
	changed = append(changed, todoChange)
	evidenceChange := state.Clone()
	evidenceChange.Evidence = []string{"新增证据"}
	changed = append(changed, evidenceChange)
	phaseChange := state.Clone()
	phaseChange.Phase = taskstate.PhasePaused
	changed = append(changed, phaseChange)
	objectiveChange := state.Clone()
	objectiveChange.Objective = "新的目标"
	changed = append(changed, objectiveChange)
	blockedChange := state.Clone()
	blockedChange.Phase = taskstate.PhaseBlocked
	blockedChange.BlockedReason = &taskstate.BlockedReason{Code: "model-reported", Message: "条件"}
	changed = append(changed, blockedChange)

	for i, candidate := range changed {
		if !TaskStateChanged(&previous, candidate) {
			t.Fatalf("change %d should re-inject task state", i)
		}
	}

	// First state is injected.
	if !TaskStateChanged(nil, state) {
		t.Fatal("first task state should be injected")
	}
	// Digest stability across clones.
	if TaskStateDigest(state) != TaskStateDigest(state.Clone()) {
		t.Fatal("digest must be stable across clones")
	}
}

func TestTaskStateContextDisabledForPlainChat(t *testing.T) {
	// A plain chat turn has no task state; the renderer must produce nothing,
	// and the change detector must not request an injection.
	if got := RenderTaskState(taskstate.State{}); got != "" {
		t.Fatalf("plain chat rendered %q, want empty", got)
	}
	if TaskStateChanged(nil, taskstate.State{}) {
		t.Fatal("plain chat must not inject task state")
	}
}
