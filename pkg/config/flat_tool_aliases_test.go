package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigForTest(t *testing.T, raw string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	return LoadConfig(path)
}

// TestLoadConfigFlatToolAliases 锁定外部配置界面的扁平工具开关兼容：
// 这些键不再触发 unknown field，并映射到对应的分组配置。
func TestLoadConfigFlatToolAliases(t *testing.T) {
	cfg, err := loadConfigForTest(t, `{
  "version": 2,
  "tools": {
    "glob": {"enabled": false},
    "grep": {"enabled": true},
    "job_kill": {"enabled": false},
    "create_goal": {"enabled": false},
    "session_search": {"enabled": false},
    "list_agents": {"enabled": false},
    "read_mcp_resource": {"enabled": false},
    "task_get": {"enabled": false},
    "task_update": {"enabled": true},
    "todo_write": {"enabled": false},
    "skill": {"enabled": false},
    "delegate": {"enabled": false}
  }
}`)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.Tools.DSH.IsGlobEnabled() {
		t.Fatal("glob should be disabled by flat alias")
	}
	if !cfg.Tools.DSH.IsGrepEnabled() {
		t.Fatal("grep should stay enabled (flat alias true)")
	}
	if cfg.Tools.DSH.IsJobsEnabled() {
		t.Fatal("jobs group should be disabled when job_kill is false")
	}
	if cfg.Tools.DSH.IsGoalEnabled() {
		t.Fatal("goal group should be disabled when create_goal is false")
	}
	if cfg.Tools.DSH.IsSessionQueryEnabled() {
		t.Fatal("session query group should be disabled when session_search is false")
	}
	if cfg.Tools.DSH.IsSubagentControlEnabled() {
		t.Fatal("subagent control group should be disabled when list_agents is false")
	}
	if cfg.Tools.DSH.IsMCPResourcesEnabled() {
		t.Fatal("MCP resources group should be disabled when read_mcp_resource is false")
	}
	if cfg.Tools.Task.Enabled {
		t.Fatal("task tools should be disabled when task_get is false")
	}
	if cfg.Tools.Todo.Enabled {
		t.Fatal("todo tool should be disabled when todo_write is false")
	}
	if cfg.Tools.SkillLoad.Enabled {
		t.Fatal("skill tool should be disabled when skill is false")
	}
	if cfg.Tools.IsDelegateEnabled() {
		t.Fatal("delegate should be disabled by flat alias")
	}
}

// TestLoadConfigFlatToolAliasesAllTrue 确保扁平开关全为 true 时不会覆盖
// 默认启用状态，也不会改变 delegate 的多 agent 自动注册契约。
func TestLoadConfigFlatToolAliasesAllTrue(t *testing.T) {
	cfg, err := loadConfigForTest(t, `{
  "version": 2,
  "tools": {
    "glob": {"enabled": true},
    "grep": {"enabled": true},
    "job_run": {"enabled": true},
    "job_list": {"enabled": true},
    "job_output": {"enabled": true},
    "job_kill": {"enabled": true},
    "create_goal": {"enabled": true},
    "get_goal": {"enabled": true},
    "update_goal": {"enabled": true},
    "session_search": {"enabled": true},
    "session_trace": {"enabled": true},
    "list_agents": {"enabled": true},
    "wait_agent": {"enabled": true},
    "interrupt_agent": {"enabled": true},
    "list_mcp_resources": {"enabled": true},
    "list_mcp_resource_templates": {"enabled": true},
    "read_mcp_resource": {"enabled": true},
    "task_get": {"enabled": true},
    "task_update": {"enabled": true},
    "todo_write": {"enabled": true},
    "skill": {"enabled": true},
    "delegate": {"enabled": true}
  }
}`)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if !cfg.Tools.DSH.IsGlobEnabled() || !cfg.Tools.DSH.IsGrepEnabled() ||
		!cfg.Tools.DSH.IsJobsEnabled() || !cfg.Tools.DSH.IsGoalEnabled() ||
		!cfg.Tools.DSH.IsSessionQueryEnabled() || !cfg.Tools.DSH.IsSubagentControlEnabled() ||
		!cfg.Tools.DSH.IsMCPResourcesEnabled() {
		t.Fatalf("DSH groups should stay enabled: %+v", cfg.Tools.DSH)
	}
	if !cfg.Tools.Task.Enabled || !cfg.Tools.Todo.Enabled || !cfg.Tools.SkillLoad.Enabled {
		t.Fatalf("task/todo/skill should stay enabled: %+v", cfg.Tools)
	}
	if !cfg.Tools.IsDelegateEnabled() {
		t.Fatal("delegate should stay enabled by default")
	}
}

// TestLoadConfigUnknownFlatToolStillRejected 确保兼容映射不会放松未知字段校验。
func TestLoadConfigUnknownFlatToolStillRejected(t *testing.T) {
	_, err := loadConfigForTest(t, `{
  "version": 2,
  "tools": {
    "not_a_real_tool": {"enabled": true}
  }
}`)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "tools.not_a_real_tool") {
		t.Fatalf("error should name the unknown field, got %q", err.Error())
	}
}
