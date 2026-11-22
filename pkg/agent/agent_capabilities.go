package agent

import (
	"context"
	"os"
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
	MCPServers   []CapabilityMCPServer `json:"mcp_servers"`
	ReadPaths    []string              `json:"read_paths"`
	WritePaths   []string              `json:"write_paths"`
	ReportedAt   time.Time             `json:"reported_at"`
	MCPInitError string                `json:"mcp_init_error,omitempty"`
}

type CapabilityMCPServer struct {
	Name      string   `json:"name"`
	Connected bool     `json:"connected"`
	Tools     []string `json:"tools"`
}

// CapabilitySnapshot 返回当前 AgentLoop 实际加载的工具、MCP 工具和路径权限。
// MCP 初始化在首次调用时完成，以便启动上报不只包含静态 builtin tools。
func (al *AgentLoop) CapabilitySnapshot(ctx context.Context) CapabilitySnapshot {
	snapshot := CapabilitySnapshot{ReportedAt: time.Now(), PID: os.Getpid()}
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
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		snapshot.MCPInitError = err.Error()
	}
	if agent := al.registry.GetDefaultAgent(); agent != nil && agent.Tools != nil {
		snapshot.Tools = agent.Tools.List()
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
