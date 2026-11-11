package integrationtools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/skills"
)

// FindSkillsTool 允许 LLM agent 从技能注册表中搜索可安装的技能。
type FindSkillsTool struct {
	registryMgr *skills.RegistryManager
	cache       *skills.SearchCache
}

// NewFindSkillsTool 创建一个新的 FindSkillsTool。
// registryMgr 是共享的注册表管理器（在 createToolRegistry 中由配置构建）。
// cache 是用于对相似查询去重的搜索缓存。
func NewFindSkillsTool(registryMgr *skills.RegistryManager, cache *skills.SearchCache) *FindSkillsTool {
	return &FindSkillsTool{
		registryMgr: registryMgr,
		cache:       cache,
	}
}

func (t *FindSkillsTool) Name() string {
	return "find_skills"
}

func (t *FindSkillsTool) Description() string {
	return "从技能注册表搜索可安装的技能。返回技能 slug、描述、版本和相关性分数。在调用 install_skill 安装前，先用它发现可用技能。"
}

func (t *FindSkillsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "描述所需技能能力的搜索词（例如 'github integration'、'database management'）",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "最多返回的结果数（1-20，默认 5）",
				"minimum":     1.0,
				"maximum":     20.0,
			},
		},
		"required": []string{"query"},
	}
}

func (t *FindSkillsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	if !ok || query == "" {
		return ErrorResult("query 为必填项，且必须是非空字符串")
	}

	limit := 5
	if l, ok := args["limit"].(float64); ok {
		li := int(l)
		if li >= 1 && li <= 20 {
			limit = li
		}
	}

	// 先查缓存。
	if t.cache != nil {
		if cached, hit := t.cache.Get(query); hit {
			return SilentResult(formatSearchResults(query, cached, true))
		}
	}

	// 搜索所有注册表。
	results, err := t.registryMgr.SearchAll(ctx, query, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("技能搜索失败：%v", err))
	}

	// 缓存结果。
	if t.cache != nil && len(results) > 0 {
		t.cache.Put(query, results)
	}

	return SilentResult(formatSearchResults(query, results, false))
}

func formatSearchResults(query string, results []skills.SearchResult, cached bool) string {
	if len(results) == 0 {
		return fmt.Sprintf("未找到与查询匹配的技能：%q", query)
	}

	var sb strings.Builder
	source := ""
	if cached {
		source = "（缓存）"
	}
	sb.WriteString(fmt.Sprintf("找到 %d 个与 %q 匹配的技能%s：\n\n", len(results), query, source))

	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. **%s**", i+1, r.Slug))
		if r.Version != "" {
			sb.WriteString(fmt.Sprintf(" v%s", r.Version))
		}
		sb.WriteString(fmt.Sprintf("  （得分: %.3f，注册表: %s）\n", r.Score, r.RegistryName))
		if r.DisplayName != "" && r.DisplayName != r.Slug {
			sb.WriteString(fmt.Sprintf("   名称：%s\n", r.DisplayName))
		}
		if r.Summary != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", r.Summary))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("使用 install_skill 并传入 slug 即可安装对应技能。")
	return sb.String()
}
