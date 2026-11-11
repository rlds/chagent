package agent

import (
	"sync"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/routing"
	"github.com/rlds/chagent/pkg/tools"
)

// AgentRegistry 管理多个 agent 实例并将消息路由到它们。
type AgentRegistry struct {
	cfg      *config.Config
	agents   map[string]*AgentInstance
	resolver *routing.RouteResolver
	mu       sync.RWMutex
}

// NewAgentRegistry 根据配置创建注册表，并实例化所有 agent。
func NewAgentRegistry(
	cfg *config.Config,
	provider providers.LLMProvider,
) *AgentRegistry {
	registry := &AgentRegistry{
		cfg:      cfg,
		agents:   make(map[string]*AgentInstance),
		resolver: routing.NewRouteResolver(cfg),
	}

	agentConfigs := cfg.Agents.List
	if len(agentConfigs) == 0 {
		implicitAgent := &config.AgentConfig{
			ID:      "main",
			Default: true,
		}
		instance := NewAgentInstance(implicitAgent, &cfg.Agents.Defaults, cfg, provider)
		registry.agents["main"] = instance
		logger.InfoCF("agent", "Created implicit main agent (no agents.list configured)", nil)
	} else {
		for i := range agentConfigs {
			ac := &agentConfigs[i]
			id := routing.NormalizeAgentID(ac.ID)
			instance := NewAgentInstance(ac, &cfg.Agents.Defaults, cfg, provider)
			registry.agents[id] = instance
			logger.InfoCF("agent", "Registered agent",
				map[string]any{
					"agent_id":  id,
					"name":      ac.Name,
					"workspace": instance.Workspace,
					"model":     instance.Model,
				})
		}
	}

	for _, instance := range registry.agents {
		if instance.ContextBuilder != nil {
			instance.ContextBuilder.WithAgentDiscovery(instance.ID, registry.ListSpawnableAgents)
		}
	}

	return registry
}

// GetAgent 返回给定 ID 对应的 agent 实例。
func (r *AgentRegistry) GetAgent(agentID string) (*AgentInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id := routing.NormalizeAgentID(agentID)
	agent, ok := r.agents[id]
	return agent, ok
}

// ResolveRoute 确定由哪个 agent 处理规范化后的入站上下文。
func (r *AgentRegistry) ResolveRoute(inbound bus.InboundContext) routing.ResolvedRoute {
	return r.resolver.ResolveRoute(inbound)
}

// ListAgentIDs 返回所有已注册的 agent ID。
func (r *AgentRegistry) ListAgentIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	return ids
}

func (r *AgentRegistry) allowedMCPServers() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.agents) == 0 {
		return nil
	}

	union := make(map[string]struct{})
	for _, agent := range r.agents {
		if agent == nil {
			continue
		}
		if agent.MCPServerAllowlist == nil {
			return nil
		}
		for serverName := range agent.MCPServerAllowlist {
			union[serverName] = struct{}{}
		}
	}

	return union
}

// CanSpawnSubagent 检查 parentAgentID 是否允许生成 targetAgentID。
func (r *AgentRegistry) CanSpawnSubagent(parentAgentID, targetAgentID string) bool {
	parent, ok := r.GetAgent(parentAgentID)
	if !ok {
		return false
	}
	return agentAllowsSubagent(parent, routing.NormalizeAgentID(targetAgentID))
}

func agentAllowsSubagent(parent *AgentInstance, targetNorm string) bool {
	if parent == nil || parent.Subagents == nil || parent.Subagents.AllowAgents == nil {
		return false
	}
	for _, allowed := range parent.Subagents.AllowAgents {
		if allowed == "*" {
			return true
		}
		if routing.NormalizeAgentID(allowed) == targetNorm {
			return true
		}
	}
	return false
}

func agentHasSpawnTool(agent *AgentInstance) bool {
	if agent == nil || agent.Tools == nil {
		return false
	}
	_, ok := agent.Tools.Get("spawn")
	return ok
}

// ForEachTool 对所有 agent 中以给定名称注册的每个工具调用 fn。
// 这适合在注册表构建完成后向工具传播依赖（例如
// MediaStore）。
func (r *AgentRegistry) ForEachTool(name string, fn func(tools.Tool)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, agent := range r.agents {
		if t, ok := agent.Tools.Get(name); ok {
			fn(t)
		}
	}
}

// Close 释放所有已注册 agent 持有的资源。
func (r *AgentRegistry) Close() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, agent := range r.agents {
		if err := agent.Close(); err != nil {
			logger.WarnCF("agent", "Failed to close agent",
				map[string]any{"agent_id": agent.ID, "error": err.Error()})
		}
	}
}

// GetDefaultAgent 返回默认 agent 实例。
func (r *AgentRegistry) GetDefaultAgent() *AgentInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id := r.defaultAgentIDLocked(); id != "" {
		if agent, ok := r.agents[id]; ok {
			return agent
		}
	}
	for id := range r.agents {
		return r.agents[id]
	}
	return nil
}

// CloseBackgroundJobs 终止注册表中每个 agent 拥有的后台 DSH 任务。
// 它用于配置重载时，避免泄漏上一代的长运行子进程；agent 会话保持打开。
func (r *AgentRegistry) CloseBackgroundJobs() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, agent := range r.agents {
		if agent == nil || agent.DSHJobs == nil {
			continue
		}
		if err := agent.DSHJobs.Close(); err != nil {
			logger.WarnCF("agent", "Failed to close DSH jobs on reload",
				map[string]any{"agent_id": agent.ID, "error": err.Error()})
		}
		agent.DSHJobs = nil
	}
}
