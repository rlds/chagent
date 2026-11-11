package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// DSH jobs 工具对外声明的提示层/槽位/来源。它们由 DSH 工具组注册，
// 并作为能力类工具渲染。
const (
	PromptLayer  = toolshared.ToolPromptLayerCapability
	PromptSlot   = "dsh"
	PromptSource = "tool_registry:dsh"
)

const (
	toolNameJobRun    = "job_run"
	toolNameJobList   = "job_list"
	toolNameJobOutput = "job_output"
	toolNameJobKill   = "job_kill"

	defaultListLimit = 20
)

// dshPromptMetadata 由四个 jobs 工具共享。
func dshPromptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{Layer: PromptLayer, Slot: PromptSlot, Source: PromptSource}
}

// managerError 把 manager 初始化失败渲染为工具错误。
func managerError(name string) *toolshared.ToolResult {
	return toolshared.ErrorResult(fmt.Sprintf(
		"错误：%s 不可用：jobs manager 需要有效的工作区 workspace", name))
}

func marshalResult(value any) *toolshared.ToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return toolshared.ErrorResult("错误：无法编码 job 结果：" + err.Error())
	}
	return toolshared.NewToolResult(string(data))
}

func toolError(action string, err error) *toolshared.ToolResult {
	if err == nil {
		return toolshared.ErrorResult("错误：" + action)
	}
	return toolshared.ErrorResult("错误：" + action + "：" + err.Error())
}

// jobRunEnvelope 是 job_run 的结果结构。
type jobRunEnvelope struct {
	Job  *JobView `json:"job"`
	Hint string   `json:"hint"`
}

// jobListEnvelope 是 job_list 的结果结构。
type jobListEnvelope struct {
	Jobs      []JobView `json:"jobs"`
	Count     int       `json:"count"`
	Running   int       `json:"running"`
	Limit     int       `json:"limit"`
	Truncated bool      `json:"truncated"`
}

// jobKillEnvelope 是 job_kill 的结果结构。
type jobKillEnvelope struct {
	Job     *JobView `json:"job"`
	Killed  bool     `json:"killed"`
	Message string   `json:"message,omitempty"`
}

// jobOutputEnvelope 是 job_output 的结果结构（在 OutputResult 基础上
// 附带给人看的提示）。
type jobOutputEnvelope struct {
	*OutputResult
	Hint string `json:"hint,omitempty"`
}

// --- 解析 JSON 参数的辅助函数 ---------------------------------------------

func getStringArg(args map[string]any, key string) (string, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("%s 必须是字符串，实际为 %T", key, raw)
	}
	return value, true, nil
}

func getIntArg(args map[string]any, key string) (int, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return 0, false, nil
	}
	switch value := raw.(type) {
	case float64:
		if math.Trunc(value) != value {
			return 0, false, fmt.Errorf("%s 必须是整数，实际为 %v", key, value)
		}
		if value > math.MaxInt32 || value < math.MinInt32 {
			return 0, false, fmt.Errorf("%s 超出范围：%v", key, value)
		}
		return int(value), true, nil
	case int:
		return value, true, nil
	case int64:
		return int(value), true, nil
	default:
		return 0, false, fmt.Errorf("%s 必须是整数，实际为 %T", key, raw)
	}
}

func getBoolArg(args map[string]any, key string) (bool, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return false, false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s 必须是布尔值，实际为 %T", key, raw)
	}
	return value, true, nil
}

// --- job_run ---------------------------------------------------------------

// JobRunTool 启动一条后台 shell 命令并返回其 job id。
type JobRunTool struct{ manager *Manager }

// NewJobRunTool 返回绑定到 m 的 job_run 工具。
func NewJobRunTool(m *Manager) *JobRunTool { return &JobRunTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *JobRunTool) Name() string { return toolNameJobRun }

// Description 实现 toolshared.Tool。
func (t *JobRunTool) Description() string {
	return "在工作区后台启动一条 shell 命令，立即返回 job_id。长命令（构建、测试、训练、下载）用本工具而不是阻塞式 exec；" +
		"之后用 job_output 读取增量/尾部输出、job_list 查看状态、job_kill 终止（按进程组 SIGTERM→SIGKILL）。" +
		"后台命令要与前台解耦：不要等待交互式输入，stdin 已关闭。完成不会主动通知，需要结果时用 job_output 的 wait。"
}

// Parameters 实现 toolshared.Tool。
func (t *JobRunTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "要在工作区后台执行的 shell 命令（非交互式；stdin 关闭）",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "可选：该 job 的最长运行毫秒数；超时后按 killed 结束并标记 timed_out。不写则用 manager 默认（通常无超时）",
			},
		},
		"required": []string{"command"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *JobRunTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *JobRunTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t.manager == nil {
		return managerError(t.Name())
	}
	command, _, err := getStringArg(args, "command")
	if err != nil {
		return toolError("job_run 参数无效", err)
	}
	timeoutMs, hasTimeout, err := getIntArg(args, "timeout_ms")
	if err != nil {
		return toolError("job_run 参数无效", err)
	}
	var timeout time.Duration
	if hasTimeout {
		if timeoutMs < 0 {
			return toolError("job_run 参数无效", fmt.Errorf("timeout_ms 不能为负"))
		}
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	view, runErr := t.manager.RunWithTimeout(command, timeout)
	if runErr != nil {
		return toolError("job_run 失败", runErr)
	}
	return marshalResult(jobRunEnvelope{
		Job: view,
		Hint: "已启动后台 job。用 job_output(job_id, wait=true) 等待并读取结果；" +
			"不再需要的 job 用 job_kill 终止，避免占用并发额度。",
	})
}

// --- job_list --------------------------------------------------------------

// JobListTool 列出已知的后台 job，最新的在前。
type JobListTool struct{ manager *Manager }

// NewJobListTool 返回绑定到 m 的 job_list 工具。
func NewJobListTool(m *Manager) *JobListTool { return &JobListTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *JobListTool) Name() string { return toolNameJobList }

// Description 实现 toolshared.Tool。
func (t *JobListTool) Description() string {
	return "列出当前进程已知的后台 job（最新在前）：id、状态（running/stopping/completed/killed/failed）、pid、命令、开始/结束时间、退出码、输出字节数与是否截断。" +
		"可用 status 过滤。已结束的 job 只保留最近若干个。"
}

// Parameters 实现 toolshared.Tool。
func (t *JobListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type":        "string",
				"enum":        statusNames(),
				"description": "可选：只返回该状态的 job",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "可选：最多返回多少条（默认 20，最大 100）",
			},
		},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *JobListTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *JobListTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t.manager == nil {
		return managerError(t.Name())
	}
	status, _, err := getStringArg(args, "status")
	if err != nil {
		return toolError("job_list 参数无效", err)
	}
	limit, hasLimit, err := getIntArg(args, "limit")
	if err != nil {
		return toolError("job_list 参数无效", err)
	}
	if hasLimit && limit <= 0 {
		return toolError("job_list 参数无效", fmt.Errorf("limit 必须为正整数"))
	}
	if !hasLimit {
		limit = defaultListLimit
	}
	views, listErr := t.manager.List(status, limit)
	if listErr != nil {
		return toolError("job_list 失败", listErr)
	}
	running := 0
	for i := range views {
		if views[i].Status == StatusRunning || views[i].Status == StatusStopping {
			running++
		}
	}
	return marshalResult(jobListEnvelope{
		Jobs:    views,
		Count:   len(views),
		Running: running,
		Limit:   limit,
	})
}

// --- job_output ------------------------------------------------------------

// JobOutputTool 读取 job 已捕获的输出（增量或尾部）。
type JobOutputTool struct{ manager *Manager }

// NewJobOutputTool 返回绑定到 m 的 job_output 工具。
func NewJobOutputTool(m *Manager) *JobOutputTool { return &JobOutputTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *JobOutputTool) Name() string { return toolNameJobOutput }

// Description 实现 toolshared.Tool。
func (t *JobOutputTool) Description() string {
	return "读取后台 job 的输出：默认从 offset 开始的增量字节（返回 next_offset 供下次续读）；" +
		"tail_lines>0 时返回最后 N 行；wait=true 会等到 job 结束或 timeout_ms 到期（超时后 job 继续运行）。" +
		"返回同时包含 job 状态、退出码、总输出字节数与是否截断。"
}

// Parameters 实现 toolshared.Tool。
func (t *JobOutputTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job_id": map[string]any{
				"type":        "string",
				"description": "job_run 返回的 job id",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "可选：从该字节偏移开始读取（用上一次返回的 next_offset 续读；默认 0）",
			},
			"tail_lines": map[string]any{
				"type":        "integer",
				"description": "可选：返回最后 N 行（与 offset 互斥，优先 tail_lines）",
			},
			"wait": map[string]any{
				"type":        "boolean",
				"description": "可选：为 true 时阻塞等待 job 结束或超时；默认 false",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "可选：wait 的最长毫秒数（默认 30s，最大 10min）",
			},
			"max_bytes": map[string]any{
				"type":        "integer",
				"description": "可选：本次最多返回的字节数（默认 64KiB）",
			},
		},
		"required": []string{"job_id"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *JobOutputTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *JobOutputTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t.manager == nil {
		return managerError(t.Name())
	}
	jobID, _, err := getStringArg(args, "job_id")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}
	offset, hasOffset, err := getIntArg(args, "offset")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}
	tailLines, hasTail, err := getIntArg(args, "tail_lines")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}
	wait, _, err := getBoolArg(args, "wait")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}
	timeoutMs, hasTimeout, err := getIntArg(args, "timeout_ms")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}
	maxBytes, hasMaxBytes, err := getIntArg(args, "max_bytes")
	if err != nil {
		return toolError("job_output 参数无效", err)
	}

	opts := OutputOptions{Wait: wait}
	if hasOffset {
		if offset < 0 {
			return toolError("job_output 参数无效", fmt.Errorf("offset 不能为负"))
		}
		opts.Offset = int64(offset)
	}
	if hasTail {
		if tailLines < 0 {
			return toolError("job_output 参数无效", fmt.Errorf("tail_lines 不能为负"))
		}
		opts.TailLines = tailLines
	}
	if hasTimeout {
		if timeoutMs < 0 {
			return toolError("job_output 参数无效", fmt.Errorf("timeout_ms 不能为负"))
		}
		opts.Timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	if hasMaxBytes {
		if maxBytes <= 0 {
			return toolError("job_output 参数无效", fmt.Errorf("max_bytes 必须为正整数"))
		}
		opts.MaxBytes = maxBytes
	}

	result, outErr := t.manager.Output(jobID, opts)
	if outErr != nil {
		return toolError("job_output 失败", outErr)
	}
	hint := ""
	switch result.Job.Status {
	case StatusRunning, StatusStopping:
		if result.WaitedOut {
			hint = "等待超时，job 仍在运行；可再次 job_output(wait=true) 或先做其他事。"
		} else {
			hint = "job 仍在运行；可用 job_output(wait=true) 等待，或稍后按 next_offset 续读。"
		}
	default:
		if result.OutputTrunc {
			hint = "输出还有更多内容：用返回的 next_offset 继续读取。"
		} else {
			hint = "job 已结束。"
		}
	}
	return marshalResult(jobOutputEnvelope{OutputResult: result, Hint: hint})
}

// --- job_kill --------------------------------------------------------------

// JobKillTool 终止正在运行的 job 的进程组。
type JobKillTool struct{ manager *Manager }

// NewJobKillTool 返回绑定到 m 的 job_kill 工具。
func NewJobKillTool(m *Manager) *JobKillTool { return &JobKillTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *JobKillTool) Name() string { return toolNameJobKill }

// Description 实现 toolshared.Tool。
func (t *JobKillTool) Description() string {
	return "终止后台 job：向它的进程组发送 SIGTERM，超过宽限期仍存活则 SIGKILL。已结束的 job 调用本工具是安全的空操作。" +
		"不再需要的长任务应及时终止，以释放并发额度与进程资源。"
}

// Parameters 实现 toolshared.Tool。
func (t *JobKillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job_id": map[string]any{
				"type":        "string",
				"description": "要终止的 job id（job_run 返回值）",
			},
		},
		"required": []string{"job_id"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *JobKillTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *JobKillTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t.manager == nil {
		return managerError(t.Name())
	}
	jobID, _, err := getStringArg(args, "job_id")
	if err != nil {
		return toolError("job_kill 参数无效", err)
	}
	view, killErr := t.manager.Kill(jobID)
	if killErr != nil {
		return toolError("job_kill 失败", killErr)
	}
	envelope := jobKillEnvelope{
		Job:     view,
		Killed:  view.Status == StatusKilled,
		Message: "job 已终止",
	}
	if !envelope.Killed {
		envelope.Message = "job 未处于运行态；返回当前状态（空操作）"
	}
	return marshalResult(envelope)
}

// --- 构建 / 注册 ----------------------------------------------------------

// NewManagerWithTools 一次性构建 manager 及其四个工具。
func NewManagerWithTools(workspace string, opts Options) (*Manager, *JobRunTool, *JobListTool, *JobOutputTool, *JobKillTool) {
	m := NewManager(workspace, opts)
	return m, NewJobRunTool(m), NewJobListTool(m), NewJobOutputTool(m), NewJobKillTool(m)
}

// Register 在 r 上注册这四个 jobs 工具。manager 的生命周期由调用方负责，
// agent 关闭时必须调用 m.Close()（推荐的调用位置见包文档）。
func Register(r *tools.ToolRegistry, m *Manager) {
	if r == nil || m == nil {
		return
	}
	r.Register(NewJobRunTool(m))
	r.Register(NewJobListTool(m))
	r.Register(NewJobOutputTool(m))
	r.Register(NewJobKillTool(m))
}

var (
	_ toolshared.Tool                   = (*JobRunTool)(nil)
	_ toolshared.Tool                   = (*JobListTool)(nil)
	_ toolshared.Tool                   = (*JobOutputTool)(nil)
	_ toolshared.Tool                   = (*JobKillTool)(nil)
	_ toolshared.PromptMetadataProvider = (*JobRunTool)(nil)
	_ toolshared.PromptMetadataProvider = (*JobListTool)(nil)
	_ toolshared.PromptMetadataProvider = (*JobOutputTool)(nil)
	_ toolshared.PromptMetadataProvider = (*JobKillTool)(nil)
)
