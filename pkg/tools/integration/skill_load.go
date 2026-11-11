package integrationtools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/skills"
)

// SkillLoader 是技能工具所需的技能加载器子集。
type SkillLoader interface {
	ListSkills() []skills.SkillInfo
	LoadSkill(name string) (string, bool)
}

// SkillLoadTool 按需加载技能的完整指令（渐进式披露）。技能目录本身作为
// 上下文发布；本工具只返回所选中技能的主体内容。
type SkillLoadTool struct {
	loader SkillLoader
}

// NewSkillLoadTool 创建技能加载工具。
func NewSkillLoadTool(loader SkillLoader) *SkillLoadTool {
	return &SkillLoadTool{loader: loader}
}

func (t *SkillLoadTool) Name() string { return "skill" }

func (t *SkillLoadTool) Description() string {
	return "加载技能的完整指令。当任务点名或明显匹配会话技能目录中的技能时，在采取行动前调用。"
}

func (t *SkillLoadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "技能目录中的准确技能名。",
			},
		},
		"required": []string{"name"},
	}
}

func (t *SkillLoadTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrorResult("错误：技能名不能为空。")
	}
	if t == nil || t.loader == nil {
		return ErrorResult("错误：技能加载器不可用。")
	}
	body, ok := t.loader.LoadSkill(name)
	if !ok {
		return ErrorResult(fmt.Sprintf("错误：未知技能 %q，或该技能已不可用。", name))
	}
	return SilentResult("<skill_content>\n" + escapeSkillBody(body) + "\n</skill_content>")
}

func escapeSkillBody(body string) string {
	body = strings.ReplaceAll(body, "</skill_content>", `<\\/skill_content>`)
	return body
}
