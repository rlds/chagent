package config

// DSHGroupConfig 配置独立的“DSH 工具”分组。
//
// 该分组移植了 DSH 原生工具中与现有 Chagent 工具功能不完全相同的部分
// （glob/grep、后台任务、MCP 资源、会话查询、子代理控制、持久化目标）。
// 这些工具位于专用提示词组（PromptMetadata 槽位 "dsh"）中，可以按组或
// 按子组禁用。
type DSHGroupConfig struct {
	// Enabled 控制整个分组。省略或为 null 表示启用。
	Enabled *bool `json:"enabled,omitempty" yaml:"-"`

	FSSearch        DSHToolConfig `json:"fs_search"         yaml:"-"`
	Glob            DSHToolConfig `json:"glob"              yaml:"-"`
	Grep            DSHToolConfig `json:"grep"              yaml:"-"`
	Jobs            DSHToolConfig `json:"jobs"              yaml:"-"`
	MCPResources    DSHToolConfig `json:"mcp_resources"     yaml:"-"`
	SessionQuery    DSHToolConfig `json:"session_query"     yaml:"-"`
	SubagentControl DSHToolConfig `json:"subagent_control"  yaml:"-"`
	Goal            DSHToolConfig `json:"goal"              yaml:"-"`
}

// DSHToolConfig 用于开关一个 DSH 子组。省略或为 null 表示启用。
type DSHToolConfig struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"-"`
}

// IsEnabled 报告整个 DSH 分组是否启用（默认为 true）。
func (c DSHGroupConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// IsFSSearchEnabled 报告 glob/grep 子组是否启用（legacy，两者共享）。
func (c DSHGroupConfig) IsFSSearchEnabled() bool {
	return c.IsEnabled() && c.FSSearch.isEnabled()
}

// IsGlobEnabled 报告 glob 工具是否启用：glob 独立键优先，其次 fs_search（legacy）。
func (c DSHGroupConfig) IsGlobEnabled() bool {
	if !c.IsEnabled() {
		return false
	}
	if c.Glob.Enabled != nil {
		return *c.Glob.Enabled
	}
	return c.FSSearch.isEnabled()
}

// IsGrepEnabled 报告 grep 工具是否启用：grep 独立键优先，其次 fs_search（legacy）。
func (c DSHGroupConfig) IsGrepEnabled() bool {
	if !c.IsEnabled() {
		return false
	}
	if c.Grep.Enabled != nil {
		return *c.Grep.Enabled
	}
	return c.FSSearch.isEnabled()
}

// IsJobsEnabled 报告后台任务子组是否启用。
func (c DSHGroupConfig) IsJobsEnabled() bool {
	return c.IsEnabled() && c.Jobs.isEnabled()
}

// IsMCPResourcesEnabled 报告 MCP 资源子组是否启用。
func (c DSHGroupConfig) IsMCPResourcesEnabled() bool {
	return c.IsEnabled() && c.MCPResources.isEnabled()
}

// IsSessionQueryEnabled 报告会话查询子组是否启用。
func (c DSHGroupConfig) IsSessionQueryEnabled() bool {
	return c.IsEnabled() && c.SessionQuery.isEnabled()
}

// IsSubagentControlEnabled 报告子代理控制子组是否启用。
func (c DSHGroupConfig) IsSubagentControlEnabled() bool {
	return c.IsEnabled() && c.SubagentControl.isEnabled()
}

// IsGoalEnabled 报告持久化目标子组是否启用。
func (c DSHGroupConfig) IsGoalEnabled() bool {
	return c.IsEnabled() && c.Goal.isEnabled()
}

func (c DSHToolConfig) isEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}
