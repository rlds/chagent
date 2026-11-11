package config

// FlatToolAliases 接受外部配置界面按工具名输出的扁平开关
// （例如 tools.glob.enabled、tools.task_get.enabled、tools.job_run.enabled）。
//
// 这些键不是当前配置的唯一权威形状：分组配置（tools.dsh.*、tools.task、
// tools.todo、tools.skill_load）仍是标准写法。扁平键只用于兼容外部配置，
// ApplyFlatToolAliases 会按“显式 false 优先”的规则把它们归一化到分组配置；
// 省略的键不改变已有默认值。
type FlatToolAliases struct {
	CreateGoal *ToolConfig `json:"create_goal,omitempty" yaml:"-"`
	Delegate   *ToolConfig `json:"delegate,omitempty"    yaml:"-"`

	GetGoal    *ToolConfig `json:"get_goal,omitempty"    yaml:"-"`
	UpdateGoal *ToolConfig `json:"update_goal,omitempty" yaml:"-"`

	Glob *ToolConfig `json:"glob,omitempty" yaml:"-"`
	Grep *ToolConfig `json:"grep,omitempty" yaml:"-"`

	JobRun    *ToolConfig `json:"job_run,omitempty"    yaml:"-"`
	JobList   *ToolConfig `json:"job_list,omitempty"   yaml:"-"`
	JobOutput *ToolConfig `json:"job_output,omitempty" yaml:"-"`
	JobKill   *ToolConfig `json:"job_kill,omitempty"   yaml:"-"`

	ListAgents     *ToolConfig `json:"list_agents,omitempty"     yaml:"-"`
	WaitAgent      *ToolConfig `json:"wait_agent,omitempty"      yaml:"-"`
	InterruptAgent *ToolConfig `json:"interrupt_agent,omitempty" yaml:"-"`

	ListMCPResources         *ToolConfig `json:"list_mcp_resources,omitempty"          yaml:"-"`
	ListMCPResourceTemplates *ToolConfig `json:"list_mcp_resource_templates,omitempty" yaml:"-"`
	ReadMCPResource          *ToolConfig `json:"read_mcp_resource,omitempty"           yaml:"-"`

	SessionSearch *ToolConfig `json:"session_search,omitempty" yaml:"-"`
	SessionTrace  *ToolConfig `json:"session_trace,omitempty"  yaml:"-"`

	TaskGet    *ToolConfig `json:"task_get,omitempty"    yaml:"-"`
	TaskUpdate *ToolConfig `json:"task_update,omitempty" yaml:"-"`
	TodoWrite  *ToolConfig `json:"todo_write,omitempty"  yaml:"-"`
	Skill      *ToolConfig `json:"skill,omitempty"       yaml:"-"`
}

// ApplyFlatToolAliases 把扁平工具开关归一化到标准分组配置。
//
// 规则：
//   - 只有显式 false 会改变结果，避免“补一个 true”覆盖嵌套配置里的显式关闭；
//   - 同一分组内任一扁平开关为 false，就关闭该分组（分组配置不支持单工具粒度）；
//   - delegate 只记录显式关闭，默认仍由“多 agent 时自动注册”的行为决定。
func (t *ToolsConfig) ApplyFlatToolAliases() {
	if t == nil {
		return
	}

	t.Task.Enabled = flatToolEnabled(t.Task.Enabled, t.TaskGet, t.TaskUpdate)
	t.Todo.Enabled = flatToolEnabled(t.Todo.Enabled, t.TodoWrite)
	t.SkillLoad.Enabled = flatToolEnabled(t.SkillLoad.Enabled, t.Skill)

	t.DSH.Glob.Enabled = flatGroupEnabled(t.DSH.Glob.Enabled, t.Glob)
	t.DSH.Grep.Enabled = flatGroupEnabled(t.DSH.Grep.Enabled, t.Grep)
	t.DSH.Jobs.Enabled = flatGroupEnabled(t.DSH.Jobs.Enabled, t.JobRun, t.JobList, t.JobOutput, t.JobKill)
	t.DSH.Goal.Enabled = flatGroupEnabled(t.DSH.Goal.Enabled, t.CreateGoal, t.GetGoal, t.UpdateGoal)
	t.DSH.SessionQuery.Enabled = flatGroupEnabled(t.DSH.SessionQuery.Enabled, t.SessionSearch, t.SessionTrace)
	t.DSH.SubagentControl.Enabled = flatGroupEnabled(
		t.DSH.SubagentControl.Enabled, t.ListAgents, t.WaitAgent, t.InterruptAgent,
	)
	t.DSH.MCPResources.Enabled = flatGroupEnabled(
		t.DSH.MCPResources.Enabled, t.ListMCPResources, t.ListMCPResourceTemplates, t.ReadMCPResource,
	)

	if t.Delegate != nil && !t.Delegate.Enabled {
		t.delegateDisabled = true
	}
}

// IsDelegateEnabled 报告 delegate 工具是否允许在多 agent 环境中注册。
// 默认允许；只有扁平配置 tools.delegate.enabled=false 时才关闭。
func (t *ToolsConfig) IsDelegateEnabled() bool {
	return t == nil || !t.delegateDisabled
}

func flatToolEnabled(current bool, aliases ...*ToolConfig) bool {
	for _, alias := range aliases {
		if alias != nil && !alias.Enabled {
			return false
		}
	}
	return current
}

func flatGroupEnabled(current *bool, aliases ...*ToolConfig) *bool {
	for _, alias := range aliases {
		if alias != nil && !alias.Enabled {
			disabled := false
			return &disabled
		}
	}
	return current
}
