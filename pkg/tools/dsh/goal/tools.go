package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// DSH goal 工具对外声明的提示层/槽位/来源。它们由 DSH 工具组注册，
// 并作为能力工具（capability tooling）呈现。
const (
	PromptLayer  = toolshared.ToolPromptLayerCapability
	PromptSlot   = "dsh"
	PromptSource = "tool_registry:dsh"
)

const (
	toolNameCreateGoal = "create_goal"
	toolNameGetGoal    = "get_goal"
	toolNameUpdateGoal = "update_goal"

	defaultListLimit = 20
)

// dshPromptMetadata 由三个 goal 工具共用。
func dshPromptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{Layer: PromptLayer, Slot: PromptSlot, Source: PromptSource}
}

// managerError 把 manager 初始化失败渲染为工具错误。
func managerError(name string) *toolshared.ToolResult {
	return toolshared.ErrorResult(fmt.Sprintf("错误：%s 不可用：goal manager 未初始化（需要工作区 workspace）", name))
}

// goalView 是面向模型的稳定 JSON 结构，用于描述单个 goal。
type goalView struct {
	ID        string   `json:"id"`
	Objective string   `json:"objective"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Evidence  []string `json:"evidence"`
	Note      string   `json:"note,omitempty"`
}

type goalEnvelope struct {
	Goal *goalView `json:"goal"`
}

type goalListEnvelope struct {
	Goals     []Summary     `json:"goals"`
	Count     int           `json:"count"`
	Limit     int           `json:"limit"`
	Truncated bool          `json:"truncated"`
	Skipped   []SkippedFile `json:"skipped,omitempty"`
}

func toGoalView(goal Goal) *goalView {
	evidence := append([]string{}, goal.Evidence...)
	if evidence == nil {
		evidence = []string{}
	}
	return &goalView{
		ID:        goal.ID,
		Objective: goal.Objective,
		Status:    goal.Status,
		CreatedAt: goal.CreatedAt,
		UpdatedAt: goal.UpdatedAt,
		Evidence:  evidence,
		Note:      goal.Note,
	}
}

func marshalResult(value any) *toolshared.ToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return toolshared.ErrorResult("错误：无法编码 goal 结果：" + err.Error())
	}
	return toolshared.NewToolResult(string(data))
}

func toolError(action string, err error) *toolshared.ToolResult {
	if err == nil {
		return toolshared.ErrorResult("错误：" + action)
	}
	return toolshared.ErrorResult("错误：" + action + "：" + err.Error())
}

// getStringArg 读取一个可选的字符串参数。
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

// getIntArg 从 JSON 解码后的输入中读取一个可选的整数参数。
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

// getStringSliceArg 读取一个可选的字符串数组参数；为便于模型使用，
// 单个字符串会被接受为只含一个元素的列表。
func getStringSliceArg(args map[string]any, key string) ([]string, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return nil, false, nil
	}
	switch value := raw.(type) {
	case string:
		return []string{value}, true, nil
	case []string:
		return append([]string{}, value...), true, nil
	case []any:
		out := make([]string, 0, len(value))
		for i, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, false, fmt.Errorf("%s[%d] 必须是字符串，实际为 %T", key, i, item)
			}
			out = append(out, text)
		}
		return out, true, nil
	default:
		return nil, false, fmt.Errorf("%s 必须是字符串数组，实际为 %T", key, raw)
	}
}

// CreateGoalTool 实现 `create_goal`：持久化一个新的 active 目标。
type CreateGoalTool struct {
	manager *Manager
}

// NewCreateGoalTool 基于 m 返回一个 create_goal 工具。
func NewCreateGoalTool(m *Manager) *CreateGoalTool { return &CreateGoalTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *CreateGoalTool) Name() string { return toolNameCreateGoal }

// Description 实现 toolshared.Tool。
func (t *CreateGoalTool) Description() string {
	return "创建一个持久化目标（goal），使其跨回合/重启存续，并可在后续 update_goal 中推进状态。" +
		"当直接用户请求是长期目标时使用（用户不一定说“目标”）；单回合小任务不要使用。返回新建目标的 id。"
}

// Parameters 实现 toolshared.Tool。
func (t *CreateGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"objective": map[string]any{
				"type":        "string",
				"description": "目标的具体完成条件（来自直接用户请求的长期目标）。",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "可选备注：范围、约束或计划摘要。",
			},
		},
		"required": []string{"objective"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *CreateGoalTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *CreateGoalTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t == nil || t.manager == nil || t.manager.Dir() == "" {
		return managerError(t.Name())
	}
	objective, _, err := getStringArg(args, "objective")
	if err != nil {
		return toolError(t.Name(), err)
	}
	note, _, err := getStringArg(args, "note")
	if err != nil {
		return toolError(t.Name(), err)
	}
	goal, err := t.manager.Create(objective, note)
	if err != nil {
		return toolError("create_goal 失败", err)
	}
	return marshalResult(goalEnvelope{Goal: toGoalView(goal)})
}

// GetGoalTool 实现 `get_goal`：读取单个目标或列出目标。
type GetGoalTool struct {
	manager *Manager
}

// NewGetGoalTool 基于 m 返回一个 get_goal 工具。
func NewGetGoalTool(m *Manager) *GetGoalTool { return &GetGoalTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *GetGoalTool) Name() string { return toolNameGetGoal }

// Description 实现 toolshared.Tool。
func (t *GetGoalTool) Description() string {
	return "读取持久化目标（goal）：给出 id 时返回该目标完整内容；省略 id 时返回最近更新目标的摘要列表（含 id/状态/证据数量）。"
}

// Parameters 实现 toolshared.Tool。
func (t *GetGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "可选：目标 id（create_goal 返回）。省略时列出全部目标摘要。",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("列表模式下最多返回多少个目标（默认 %d，最大 %d）。", defaultListLimit, ListLimit),
			},
		},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *GetGoalTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *GetGoalTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t == nil || t.manager == nil || t.manager.Dir() == "" {
		return managerError(t.Name())
	}
	id, hasID, err := getStringArg(args, "id")
	if err != nil {
		return toolError(t.Name(), err)
	}
	if hasID && strings.TrimSpace(id) != "" {
		goal, getErr := t.manager.Get(id)
		if getErr != nil {
			return toolError("get_goal 失败", getErr)
		}
		return marshalResult(goalEnvelope{Goal: toGoalView(goal)})
	}
	limit, hasLimit, err := getIntArg(args, "limit")
	if err != nil {
		return toolError(t.Name(), err)
	}
	if !hasLimit || limit <= 0 {
		limit = defaultListLimit
	}
	if limit > ListLimit {
		limit = ListLimit
	}
	goals, skipped := t.manager.LoadAll()
	total := len(goals)
	if total > limit {
		goals = goals[:limit]
	}
	return marshalResult(goalListEnvelope{
		Goals:     Summaries(goals),
		Count:     total,
		Limit:     limit,
		Truncated: total > limit,
		Skipped:   skipped,
	})
}

// UpdateGoalTool 实现 `update_goal`：迁移状态、替换备注、
// 追加证据。
type UpdateGoalTool struct {
	manager *Manager
}

// NewUpdateGoalTool 基于 m 返回一个 update_goal 工具。
func NewUpdateGoalTool(m *Manager) *UpdateGoalTool { return &UpdateGoalTool{manager: m} }

// Name 实现 toolshared.Tool。
func (t *UpdateGoalTool) Name() string { return toolNameUpdateGoal }

// Description 实现 toolshared.Tool。
func (t *UpdateGoalTool) Description() string {
	return "更新持久化目标（goal）：可迁移状态（active/complete/blocked）、替换备注、追加证据。" +
		"标记 complete 或 blocked 时必须提供 note 或 evidence 说明结果/阻塞原因；blocked 只能在具体条件持续存在时使用。"
}

// Parameters 实现 toolshared.Tool。
func (t *UpdateGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "目标 id（create_goal 或 get_goal 返回）。",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "可选：目标状态。active→complete/blocked 允许；blocked→active 表示重新打开；complete 为终态。",
				"enum":        []string{StatusActive, StatusComplete, StatusBlocked},
			},
			"note": map[string]any{
				"type":        "string",
				"description": "可选：替换目标备注。",
			},
			"evidence": map[string]any{
				"type":        "array",
				"description": "可选：追加证据（文件路径、命令结果、测试输出、用户确认等）。",
				"items": map[string]any{
					"type": "string",
				},
			},
		},
		"required": []string{"id"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider。
func (t *UpdateGoalTool) PromptMetadata() toolshared.PromptMetadata { return dshPromptMetadata() }

// Execute 实现 toolshared.Tool。
func (t *UpdateGoalTool) Execute(_ context.Context, args map[string]any) *toolshared.ToolResult {
	if t == nil || t.manager == nil || t.manager.Dir() == "" {
		return managerError(t.Name())
	}
	id, _, err := getStringArg(args, "id")
	if err != nil {
		return toolError(t.Name(), err)
	}
	if strings.TrimSpace(id) == "" {
		return toolshared.ErrorResult("错误：update_goal 需要非空 id")
	}
	status, hasStatus, err := getStringArg(args, "status")
	if err != nil {
		return toolError(t.Name(), err)
	}
	note, hasNote, err := getStringArg(args, "note")
	if err != nil {
		return toolError(t.Name(), err)
	}
	evidence, hasEvidence, err := getStringSliceArg(args, "evidence")
	if err != nil {
		return toolError(t.Name(), err)
	}
	if err := ValidateUpdateInput(status, note, evidence, hasStatus, hasNote, hasEvidence); err != nil {
		return toolError("update_goal 参数无效", err)
	}
	goal, updateErr := t.manager.Update(id, status, note, evidence, hasStatus, hasNote, hasEvidence)
	if updateErr != nil {
		return toolError("update_goal 失败", updateErr)
	}
	return marshalResult(goalEnvelope{Goal: toGoalView(goal)})
}

// NewManagerWithTools 是便捷构造器，同时返回 manager 及其三个工具
// （只需要其中一个工具时可直接使用 NewManager）。
func NewManagerWithTools(workspace string) (*Manager, *CreateGoalTool, *GetGoalTool, *UpdateGoalTool) {
	manager := NewManager(workspace)
	return manager, NewCreateGoalTool(manager), NewGetGoalTool(manager), NewUpdateGoalTool(manager)
}

// Register 把三个 DSH goal 工具注册到给定注册表。
// 传入 nil 注册表或 nil manager 时不做任何操作（该工具组是可选的）。
func Register(r *tools.ToolRegistry, m *Manager) {
	if r == nil || m == nil {
		return
	}
	r.Register(NewCreateGoalTool(m))
	r.Register(NewGetGoalTool(m))
	r.Register(NewUpdateGoalTool(m))
}
