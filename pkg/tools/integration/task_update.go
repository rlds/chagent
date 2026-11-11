package integrationtools

import (
	"context"
	"fmt"
	"time"

	"github.com/rlds/chagent/pkg/taskstate"
)

// TaskUpdateTool 实现 `task_update` 工具：面向模型的持久会话任务状态控制
// 界面。完成证据与阻塞持久化阈值都在此处执行时强制校验，
// 而不仅写在提示词里。
type TaskUpdateTool struct {
	taskStateToolBase
}

// NewTaskUpdateTool 创建一个 task_update 工具。
func NewTaskUpdateTool(opts TaskToolOptions) *TaskUpdateTool {
	return &TaskUpdateTool{taskStateToolBase: taskStateToolBase{opts: opts}}
}

// Name 实现 Tool。
func (t *TaskUpdateTool) Name() string { return "task_update" }

// Description 实现 Tool（提示词 P-13，中文）。
func (t *TaskUpdateTool) Description() string {
	return "更新当前会话的任务状态。使用 start 从明确的人类或调度器请求创建任务；使用 pause/resume/complete/block 改变阶段。恢复 blocked 任务需要直接的人类请求。没有具体证据不要标记完成。同一个条件未连续持续至少 3 轮前不要标记阻塞。"
}

// Parameters 实现 Tool（提示词 P-13）。
func (t *TaskUpdateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"description": "start、pause、resume、complete 或 block。" +
					"start 需要直接的人类或调度器请求；complete 需要证据；" +
					"block 需要同一个具体条件连续至少 3 轮。",
				"enum": []string{"start", "pause", "resume", "complete", "block"},
			},
			"objective": map[string]any{
				"type":        "string",
				"description": "目标；start 时必填，resume 时可选用于更新目标。",
			},
			"max_rounds": map[string]any{
				"type":        "integer",
				"description": "可选的轮次上限（1–200），用于 start/resume。",
			},
			"blocked_reason": map[string]any{
				"type":        "string",
				"description": "block 时必填：连续多轮持续存在的具体条件。",
			},
			"evidence": map[string]any{
				"type":        "array",
				"description": "complete 的具体证据：文件路径、测试输出、命令结果或用户确认。",
				"items": map[string]any{
					"type": "string",
				},
			},
		},
		"required": []string{"action"},
	}
}

// Execute 实现 Tool。
func (t *TaskUpdateTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	sessionKey, store, errResult := t.resolve(ctx, t.Name())
	if errResult != nil {
		return errResult
	}

	action, _, err := getStringArg(args, "action")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	state, found := loadState(store, sessionKey)
	now := t.opts.now()

	switch action {
	case "start":
		return t.start(ctx, store, sessionKey, state, found, args, now)
	case "pause":
		return t.pause(store, sessionKey, state, found, now)
	case "resume":
		return t.resume(ctx, store, sessionKey, state, found, args, now)
	case "complete":
		return t.complete(store, sessionKey, state, found, args, now)
	case "block":
		return t.block(store, sessionKey, state, found, args, now)
	case "":
		return ErrorResult("错误：无效参数：`action` 不能为空")
	default:
		return ErrorResult(fmt.Sprintf(
			"错误：无效 action：%q；必须是 start、pause、resume、complete 或 block", action,
		))
	}
}

func (t *TaskUpdateTool) start(
	ctx context.Context,
	store taskstate.Store,
	sessionKey string,
	existing taskstate.State,
	found bool,
	args map[string]any,
	now time.Time,
) *ToolResult {
	source := t.opts.authority().Source(ctx)
	if !source.AllowsStart() {
		return ErrorResult(fmt.Sprintf(
			"错误：task_update start 需要直接的人类或调度器请求（当前来源：%s）。"+
				"请等待用户明确要求后再开始任务。", source,
		))
	}

	objective, _, err := getStringArg(args, "objective")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	if objective == "" {
		return ErrorResult("错误：task_update start 需要 objective")
	}
	maxRounds, provided, err := getIntArg(args, "max_rounds")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	if !provided {
		maxRounds = t.opts.defaultMaxRounds()
	}
	if found && existing.Phase != taskstate.PhaseComplete {
		return ErrorResult(fmt.Sprintf(
			"错误：当前会话已有任务（阶段 %s）。请使用 resume 继续该任务，或先完成任务后再开始新任务。",
			existing.Phase,
		))
	}

	state, err := taskstate.New(objective, maxRounds, now)
	if err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	return NewToolResult(fmt.Sprintf(
		"任务已开始：阶段 active，轮次 0/%d，目标 %q。",
		state.MaxRounds, state.Objective,
	))
}

func (t *TaskUpdateTool) pause(
	store taskstate.Store,
	sessionKey string,
	state taskstate.State,
	found bool,
	now time.Time,
) *ToolResult {
	if !found {
		return noTaskStateError(t.Name())
	}
	if err := state.Pause(now); err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	return NewToolResult(fmt.Sprintf(
		"任务已暂停：阶段 paused，轮次 %d/%d。", state.Round, state.MaxRounds,
	))
}

func (t *TaskUpdateTool) resume(
	ctx context.Context,
	store taskstate.Store,
	sessionKey string,
	state taskstate.State,
	found bool,
	args map[string]any,
	now time.Time,
) *ToolResult {
	if !found {
		return noTaskStateError(t.Name())
	}
	objective, objectiveSet, err := getStringArg(args, "objective")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	maxRounds, maxRoundsSet, err := getIntArg(args, "max_rounds")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}

	source := t.opts.authority().Source(ctx)
	switch state.Phase {
	case taskstate.PhaseComplete:
		return ErrorResult("错误：已完成的任务不能恢复")
	case taskstate.PhaseActive:
		return ErrorResult("错误：任务已经是 active 阶段，无需恢复")
	case taskstate.PhaseBlocked:
		if !source.AllowsResumeBlocked() {
			return ErrorResult(fmt.Sprintf(
				"错误：恢复 blocked 任务需要直接的人类请求（当前来源：%s）。", source,
			))
		}
	case taskstate.PhasePaused:
		if !source.AllowsResumePaused() {
			return ErrorResult(fmt.Sprintf(
				"错误：恢复 paused 任务需要人类或调度器请求（当前来源：%s）。", source,
			))
		}
	}
	if err := state.Resume(now); err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	if objectiveSet && objective != "" {
		state.Objective = objective
	}
	if maxRoundsSet {
		if maxRounds < 1 || maxRounds > taskstate.MaxRoundsLimit {
			return ErrorResult(fmt.Sprintf(
				"错误：max_rounds 必须在 1–%d 之间，当前为 %d", taskstate.MaxRoundsLimit, maxRounds,
			))
		}
		state.MaxRounds = maxRounds
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	return NewToolResult(fmt.Sprintf(
		"任务已恢复：阶段 active，轮次 %d/%d，目标 %q。",
		state.Round, state.MaxRounds, state.Objective,
	))
}

func (t *TaskUpdateTool) complete(
	store taskstate.Store,
	sessionKey string,
	state taskstate.State,
	found bool,
	args map[string]any,
	now time.Time,
) *ToolResult {
	if !found {
		return noTaskStateError(t.Name())
	}
	evidence, _, err := getStringSliceArg(args, "evidence")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	if err := state.Complete(evidence, now); err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	return NewToolResult(fmt.Sprintf(
		"任务已完成：阶段 complete，目标 %q，已记录 %d 条证据。",
		state.Objective, len(state.Evidence),
	))
}

func (t *TaskUpdateTool) block(
	store taskstate.Store,
	sessionKey string,
	state taskstate.State,
	found bool,
	args map[string]any,
	now time.Time,
) *ToolResult {
	if !found {
		return noTaskStateError(t.Name())
	}
	message, _, err := getStringArg(args, "blocked_reason")
	if err != nil {
		return ErrorResult("错误：无效参数：" + err.Error())
	}
	if message == "" {
		return ErrorResult("错误：task_update block 需要 blocked_reason，描述连续多轮持续存在的具体条件")
	}
	threshold := t.opts.blockedThreshold()
	reason := taskstate.BlockedReason{
		Code:    taskstate.BlockedCodeModelReported,
		Message: message,
	}

	// 先记录本次尝试，这样即使尚未达到阈值，
	// 轮次计数也会被持久化。
	allowed, rounds, recordErr := state.RecordBlockerAttempt(reason, threshold, now)
	if recordErr != nil {
		return ErrorResult("错误：" + recordErr.Error())
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	if !allowed {
		return ErrorResult(fmt.Sprintf(
			"错误：标记阻塞需要同一个具体条件连续至少 %d 轮才能成立；当前条件已记录 %d 轮。"+
				"请不要在同一轮重复调用，继续推进工作或在下一轮再次报告同一条件。",
			threshold, rounds,
		))
	}
	if err := state.Block(reason, threshold, now); err != nil {
		return ErrorResult("错误：" + err.Error())
	}
	if persistResult := persistState(store, sessionKey, state); persistResult != nil {
		return persistResult
	}
	return NewToolResult(fmt.Sprintf(
		"任务已标记为阻塞：%s：%s。", reason.Code, reason.Message,
	))
}

func noTaskStateError(_ string) *ToolResult {
	return ErrorResult("错误：当前会话没有任务状态；请先调用 task_update start")
}
