// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"context"
	"fmt"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/mcp"
	"github.com/rlds/chagent/pkg/tools"
	dshmcpresources "github.com/rlds/chagent/pkg/tools/dsh/mcpresources"
)

type mcpRuntime struct {
	initOnce sync.Once
	mu       sync.Mutex
	manager  *mcp.Manager
	initErr  error
}

func (r *mcpRuntime) reset() *mcp.Manager {
	r.mu.Lock()
	manager := r.manager
	r.manager = nil
	r.initErr = nil
	r.initOnce = sync.Once{}
	r.mu.Unlock()
	return manager
}

func (r *mcpRuntime) setManager(manager *mcp.Manager) {
	r.mu.Lock()
	r.manager = manager
	r.initErr = nil
	r.mu.Unlock()
}

func (r *mcpRuntime) setInitErr(err error) {
	r.mu.Lock()
	r.initErr = err
	r.mu.Unlock()
}

func (r *mcpRuntime) getInitErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initErr
}

func (r *mcpRuntime) takeManager() *mcp.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	manager := r.manager
	r.manager = nil
	return manager
}

func (r *mcpRuntime) hasManager() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manager != nil
}

func (r *mcpRuntime) getManager() *mcp.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manager
}

// ensureMCPInitialized 只加载一次 MCP 服务器/工具，使 Run() 路径与直接调用
// agent 模式共用同一条初始化路径。
func (al *AgentLoop) ensureMCPInitialized(ctx context.Context) error {
	if !al.cfg.Tools.IsToolEnabled("mcp") {
		return nil
	}

	if al.cfg.Tools.MCP.Servers == nil || len(al.cfg.Tools.MCP.Servers) == 0 {
		logger.WarnCF("agent", "MCP is enabled but no servers are configured, skipping MCP initialization", nil)
		return nil
	}

	mcpCfg := filterMCPConfigServers(al.cfg.Tools.MCP, al.registry.allowedMCPServers())
	if mcpCfg.Servers == nil || len(mcpCfg.Servers) == 0 {
		logger.InfoCF(
			"agent",
			"No MCP servers selected after applying per-agent mcpServers allowlists",
			nil,
		)
		return nil
	}

	findValidServer := false
	for _, serverCfg := range mcpCfg.Servers {
		if serverCfg.Enabled {
			findValidServer = true
		}
	}
	if !findValidServer {
		logger.WarnCF("agent", "MCP is enabled but no valid servers are configured, skipping MCP initialization", nil)
		return nil
	}

	al.mcp.initOnce.Do(func() {
		mcpManager := mcp.NewManager(mcp.WithRuntimeEvents(al.runtimeEvents))

		defaultAgent := al.registry.GetDefaultAgent()
		workspacePath := al.cfg.WorkspacePath()
		if defaultAgent != nil && defaultAgent.Workspace != "" {
			workspacePath = defaultAgent.Workspace
		}

		if err := mcpManager.LoadFromMCPConfig(ctx, mcpCfg, workspacePath); err != nil {
			al.mcp.setInitErr(fmt.Errorf("failed to load MCP servers: %w", err))
			logger.WarnCF("agent", "Failed to load MCP servers, MCP tools will not be available",
				map[string]any{
					"error": err.Error(),
				})
			if closeErr := mcpManager.Close(); closeErr != nil {
				logger.ErrorCF("agent", "Failed to close MCP manager",
					map[string]any{
						"error": closeErr.Error(),
					})
			}
			return
		}

		// 为所有 agent 注册 MCP 工具
		servers := mcpManager.GetServers()
		uniqueTools := 0
		totalRegistrations := 0
		agentIDs := al.registry.ListAgentIDs()
		agentCount := len(agentIDs)
		registeredByServer := make(map[string]map[string]map[string]struct{}, len(servers))
		var resyncMu sync.Mutex

		for serverName, conn := range servers {
			uniqueTools += len(conn.Tools)

			// 判断该服务器的工具是否应延迟加载（隐藏）。
			// 服务器级别的 "deferred" 字段优先于全局 Discovery.Enabled。
			serverCfg := mcpCfg.Servers[serverName]
			registerAsHidden := serverIsDeferred(al.cfg.Tools.MCP.Discovery.Enabled, serverCfg)
			registeredToolsByAgent := make(map[string]map[string]struct{}, len(agentIDs))

			for _, tool := range conn.Tools {
				for _, agentID := range agentIDs {
					agent, ok := al.registry.GetAgent(agentID)
					if !ok {
						continue
					}
					if !agent.AllowsMCPServer(serverName) {
						logger.DebugCF("agent", "Skipped MCP tool registration by agent mcpServers allowlist",
							map[string]any{
								"agent_id": agentID,
								"server":   serverName,
								"tool":     tool.Name,
							})
						continue
					}

					mcpTool := tools.NewMCPTool(mcpManager, serverName, tool)
					toolName := mcpTool.Name()
					mcpTool.SetWorkspace(agent.Workspace)
					mcpTool.SetMaxInlineTextRunes(al.cfg.Tools.MCP.GetMaxInlineTextChars())
					mcpTool.SetEventPublisher(al.runtimeEvents)

					if registerAsHidden {
						agent.Tools.RegisterHidden(mcpTool)
					} else {
						agent.Tools.Register(mcpTool)
					}
					if !toolRegistryIncludes(agent.Tools, toolName) {
						continue
					}

					recordRegisteredMCPTool(registeredToolsByAgent, agentID, toolName)
					totalRegistrations++
					logger.DebugCF("agent", "Registered MCP tool",
						map[string]any{
							"agent_id": agentID,
							"server":   serverName,
							"tool":     tool.Name,
							"name":     toolName,
							"deferred": registerAsHidden,
						})
				}
			}

			for _, agentID := range agentIDs {
				agent, ok := al.registry.GetAgent(agentID)
				if !ok {
					continue
				}
				registerMCPServerPromptContributor(
					agentID,
					agent,
					serverName,
					len(registeredToolsByAgent[agentID]),
					registerAsHidden,
				)
			}
			registeredByServer[serverName] = registeredToolsByAgent
		}

		// tools/list_changed：让各 agent 的注册表与服务器的
		// 实时工具列表保持同步。新工具会即时注册；被移除的
		// 工具会被注销，避免模型继续调用已失效的名字。
		mcpManager.SetToolListChangedHandler(func(serverName string, serverTools []*sdkmcp.Tool) {
			resyncMu.Lock()
			defer resyncMu.Unlock()
			conn, ok := mcpManager.GetServers()[serverName]
			if !ok || conn == nil {
				return
			}
			serverCfg := mcpCfg.Servers[serverName]
			registerAsHidden := serverIsDeferred(al.cfg.Tools.MCP.Discovery.Enabled, serverCfg)
			previous := registeredByServer[serverName]
			next := make(map[string]map[string]struct{}, len(agentIDs))
			for _, agentID := range agentIDs {
				agent, ok := al.registry.GetAgent(agentID)
				if !ok {
					continue
				}
				if !agent.AllowsMCPServer(serverName) {
					continue
				}
				current := make(map[string]struct{}, len(serverTools))
				for _, tool := range serverTools {
					if tool == nil {
						continue
					}
					mcpTool := tools.NewMCPTool(mcpManager, serverName, tool)
					name := mcpTool.Name()
					mcpTool.SetWorkspace(agent.Workspace)
					mcpTool.SetMaxInlineTextRunes(al.cfg.Tools.MCP.GetMaxInlineTextChars())
					mcpTool.SetEventPublisher(al.runtimeEvents)
					if registerAsHidden {
						agent.Tools.RegisterHidden(mcpTool)
					} else {
						agent.Tools.Register(mcpTool)
					}
					current[name] = struct{}{}
				}
				for stale := range previous[agentID] {
					if _, keep := current[stale]; !keep {
						agent.Tools.Unregister(stale)
					}
				}
				next[agentID] = current
				registerMCPServerPromptContributor(agentID, agent, serverName, len(current), registerAsHidden)
			}
			registeredByServer[serverName] = next
			logger.InfoCF("agent", "Re-synced MCP tools after list_changed",
				map[string]any{"server": serverName, "tool_count": len(serverTools), "agents": len(next)})
		})
		logger.InfoCF("agent", "MCP tools registered successfully",
			map[string]any{
				"server_count":        len(servers),
				"unique_tools":        uniqueTools,
				"total_registrations": totalRegistrations,
				"agent_count":         agentCount,
			})

		// 仅在配置启用时才初始化发现（Discovery）工具
		if al.cfg.Tools.MCP.Active() && al.cfg.Tools.MCP.Discovery.Enabled {
			useBM25 := al.cfg.Tools.MCP.Discovery.UseBM25
			useRegex := al.cfg.Tools.MCP.Discovery.UseRegex

			// 快速失败：已启用发现功能，但没有任何搜索方式被打开
			if !useBM25 && !useRegex {
				al.mcp.setInitErr(fmt.Errorf(
					"tool discovery is enabled but neither 'use_bm25' nor 'use_regex' is set to true in the configuration",
				))
				if closeErr := mcpManager.Close(); closeErr != nil {
					logger.ErrorCF("agent", "Failed to close MCP manager",
						map[string]any{
							"error": closeErr.Error(),
						})
				}
				return
			}

			ttl := al.cfg.Tools.MCP.Discovery.TTL
			if ttl <= 0 {
				ttl = 5 // 默认值
			}

			maxSearchResults := al.cfg.Tools.MCP.Discovery.MaxSearchResults
			if maxSearchResults <= 0 {
				maxSearchResults = 5 // 默认值
			}

			logger.InfoCF("agent", "Initializing tool discovery", map[string]any{
				"bm25": useBM25, "regex": useRegex, "ttl": ttl, "max_results": maxSearchResults,
			})

			for _, agentID := range agentIDs {
				agent, ok := al.registry.GetAgent(agentID)
				if !ok {
					continue
				}
				if !agentHasDiscoverableMCPServers(al.cfg, agent.MCPServerAllowlist) {
					continue
				}

				if useRegex {
					agent.Tools.Register(tools.NewRegexSearchTool(agent.Tools, ttl, maxSearchResults))
				}
				if useBM25 {
					agent.Tools.Register(tools.NewBM25SearchTool(agent.Tools, ttl, maxSearchResults))
				}
			}
		}

		// 独立的 DSH 工具组：在同一个已连接的管理器之上
		// 提供只读的 MCP 资源工具。按 agent 分别注册，因此
		// 工作区与每个 agent 的工具白名单依然生效。
		if al.cfg.Tools.DSH.IsMCPResourcesEnabled() {
			for _, agentID := range agentIDs {
				agent, ok := al.registry.GetAgent(agentID)
				if !ok {
					continue
				}
				dshmcpresources.Register(
					agent.Tools,
					mcpManager,
					dshmcpresources.WithWorkspace(agent.Workspace),
				)
			}
			logger.InfoCF("agent", "Registered DSH MCP resource tools",
				map[string]any{"agents": len(agentIDs)})
		}

		al.mcp.setManager(mcpManager)
	})

	return al.mcp.getInitErr()
}

func registerMCPServerPromptContributor(
	agentID string,
	agent *AgentInstance,
	serverName string,
	toolCount int,
	registerAsHidden bool,
) {
	if agent == nil || agent.ContextBuilder == nil || toolCount <= 0 {
		return
	}
	if err := agent.ContextBuilder.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: serverName,
		toolCount:  toolCount,
		deferred:   registerAsHidden,
	}); err != nil {
		logger.WarnCF("agent", "Failed to register MCP prompt contributor",
			map[string]any{
				"agent_id": agentID,
				"server":   serverName,
				"error":    err.Error(),
			})
	}
}

func recordRegisteredMCPTool(
	registeredToolsByAgent map[string]map[string]struct{},
	agentID, toolName string,
) {
	if registeredToolsByAgent[agentID] == nil {
		registeredToolsByAgent[agentID] = make(map[string]struct{})
	}
	registeredToolsByAgent[agentID][toolName] = struct{}{}
}

func toolRegistryIncludes(registry *tools.ToolRegistry, name string) bool {
	if registry == nil {
		return false
	}
	return registry.HasRegistered(name)
}

func filterMCPConfigServers(
	mcpCfg config.MCPConfig,
	allowed map[string]struct{},
) config.MCPConfig {
	if allowed == nil {
		return mcpCfg
	}

	filtered := mcpCfg
	filtered.Servers = make(map[string]config.MCPServerConfig)
	normalizedAllowed := make(map[string]struct{}, len(allowed))
	for serverName := range allowed {
		name := normalizeMCPServerName(serverName)
		if name == "" {
			continue
		}
		normalizedAllowed[name] = struct{}{}
	}
	for serverName, serverCfg := range mcpCfg.Servers {
		if _, ok := normalizedAllowed[normalizeMCPServerName(serverName)]; ok {
			filtered.Servers[serverName] = serverCfg
		}
	}

	return filtered
}

func agentHasDiscoverableMCPServers(cfg *config.Config, allowed map[string]struct{}) bool {
	if cfg == nil || !cfg.Tools.MCP.Active() || !cfg.Tools.MCP.Discovery.Enabled {
		return false
	}

	filtered := filterMCPConfigServers(cfg.Tools.MCP, allowed)
	for _, serverCfg := range filtered.Servers {
		if serverCfg.Enabled && serverIsDeferred(cfg.Tools.MCP.Discovery.Enabled, serverCfg) {
			return true
		}
	}

	return false
}

// serverIsDeferred 报告某个 MCP 服务器的工具是否应注册为隐藏
// （延迟加载/发现模式）。
//
// 服务器级别的 Deferred 字段优先于全局 discoveryEnabled 默认值。
// 当 Deferred 为 nil 时，回退使用 discoveryEnabled。
func serverIsDeferred(discoveryEnabled bool, serverCfg config.MCPServerConfig) bool {
	if !discoveryEnabled {
		return false
	}
	if serverCfg.Deferred != nil {
		return *serverCfg.Deferred
	}
	return true
}
