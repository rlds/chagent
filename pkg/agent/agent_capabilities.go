package agent

import (
	"context"
	"os"
	"strings"
	"time"
)

// CapabilitySnapshot 是 Client/Server 可读取的 Agent 真实运行能力快照。
// 工具和 MCP 数据来自当前进程的 registry/manager，而不是静态配置文件。
type CapabilitySnapshot struct {
	AgentType    string                `json:"agent_type"`
	Version      string                `json:"version"`
	Model        string                `json:"model"`
	Workspace    string                `json:"workspace"`
	PID          int                   `json:"pid"`
	Tools        []string              `json:"tools"`
	Skills       []CapabilitySkill     `json:"skills"`
	MCPServers   []CapabilityMCPServer `json:"mcp_servers"`
	ReadPaths    []string              `json:"read_paths"`
	WritePaths   []string              `json:"write_paths"`
	ReportedAt   time.Time             `json:"reported_at"`
	MCPInitError string                `json:"mcp_init_error,omitempty"`
	// MaxParallelTurns 是本进程实际加载的并发轮次上限；
	// ActiveSessions 是当前有活动 Turn 的会话数。用于发布/运维核对
	// “配置允许并行”与“实际并行”是否一致。
	MaxParallelTurns int `json:"max_parallel_turns"`
	ActiveSessions   int `json:"active_sessions"`
	// InboundIdempotent 表示入站 message_id 幂等：Client 在连接异常后可以安全
	// 用同一 message_id 重投，服务端只回 ack、不重复执行。Client 用它决定是否
	// 对 1006/EOF 做自动重投（旧 Agent 不支持时必须保守处理）。
	InboundIdempotent bool `json:"inbound_idempotent"`
}

type CapabilitySkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
}

type CapabilityMCPServer struct {
	Name      string   `json:"name"`
	Connected bool     `json:"connected"`
	Tools     []string `json:"tools"`
}

// CapabilitySnapshot 返回当前 AgentLoop 实际加载的工具、MCP 工具和路径权限。
// MCP 初始化在首次调用时完成，以便启动上报不只包含静态 builtin tools。
func (al *AgentLoop) CapabilitySnapshot(ctx context.Context) CapabilitySnapshot {
	snapshot := CapabilitySnapshot{ReportedAt: time.Now(), PID: os.Getpid(), InboundIdempotent: true}
	if al == nil {
		return snapshot
	}
	cfg := al.GetConfig()
	if cfg != nil {
		snapshot.Version = cfg.BuildInfo.Version
		snapshot.Model = cfg.Agents.Defaults.ModelName
		snapshot.Workspace = cfg.WorkspacePath()
		snapshot.ReadPaths = append([]string{}, cfg.Tools.AllowReadPaths...)
		snapshot.WritePaths = append([]string{}, cfg.Tools.AllowWritePaths...)
		snapshot.MaxParallelTurns = cfg.Agents.Defaults.MaxParallelTurns
	}
	snapshot.ActiveSessions = al.activeSessionCount()
	if err := al.ensureMCPInitialized(ctx); err != nil {
		snapshot.MCPInitError = err.Error()
	}
	if agent := al.registry.GetDefaultAgent(); agent != nil {
		if agent.Tools != nil {
			snapshot.Tools = agent.Tools.List()
		}
		if agent.ContextBuilder != nil {
			if loader := agent.ContextBuilder.SkillLoader(); loader != nil {
				for _, skill := range loader.ListSkills() {
					if len(agent.SkillsFilter) > 0 && !containsStringFold(agent.SkillsFilter, skill.Name) {
						continue
					}
					snapshot.Skills = append(snapshot.Skills, CapabilitySkill{
						Name: skill.Name, Description: skill.Description, Source: skill.Source,
					})
				}
			}
		}
	}
	if manager := al.mcp.getManager(); manager != nil {
		for name, conn := range manager.GetServers() {
			server := CapabilityMCPServer{Name: name, Connected: conn != nil}
			if conn != nil {
				for _, tool := range conn.Tools {
					if tool != nil && tool.Name != "" {
						server.Tools = append(server.Tools, tool.Name)
					}
				}
			}
			snapshot.MCPServers = append(snapshot.MCPServers, server)
		}
	}
	return snapshot
}

// activeSessionCount 统计当前注册了活动 Turn 的会话数（含 setup 占位）。
func (al *AgentLoop) activeSessionCount() int {
	count := 0
	if al == nil {
		return 0
	}
	al.activeTurnStates.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

func containsStringFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}
