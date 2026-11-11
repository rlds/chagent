package agent

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rlds/chagent/pkg/routing"
)

// AgentDescriptor 是注入到每个 agent 系统提示中的结构化发现载荷，
// 以便 LLM 能按身份选择对等 agent。
type AgentDescriptor struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListAgents 返回当前 Chagent 实例中每个 agent 的结构化描述符。
// 若提供了当前工作区，则仅用于将匹配的 agent 排在最前，以便提示更易读。
func (r *AgentRegistry) ListAgents(workspace string) []AgentDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	selfWorkspace := cleanWorkspacePath(workspace)
	descriptors := make([]AgentDescriptor, 0, len(ids))
	for _, id := range ids {
		agent := r.agents[id]
		if agent == nil {
			continue
		}
		descriptors = append(descriptors, r.buildAgentDescriptorLocked(agent))
	}

	if selfWorkspace == "" {
		return descriptors
	}

	sort.SliceStable(descriptors, func(i, j int) bool {
		leftSelf := cleanWorkspacePath(
			r.workspaceForAgentIDLocked(descriptors[i].ID),
		) == selfWorkspace
		rightSelf := cleanWorkspacePath(
			r.workspaceForAgentIDLocked(descriptors[j].ID),
		) == selfWorkspace
		if leftSelf != rightSelf {
			return leftSelf
		}
		return descriptors[i].ID < descriptors[j].ID
	})

	return descriptors
}

// ListSpawnableAgents 仅在当前 agent 可调用 spawn 时返回描述符，
// 且只包含它被允许 spawn 的对等 agent。受限的对等 agent 会被有意从发现结果中省略。
func (r *AgentRegistry) ListSpawnableAgents(agentID string) []AgentDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentID := routing.NormalizeAgentID(agentID)
	parent, ok := r.agents[parentID]
	if !ok || parent == nil {
		return nil
	}
	if !agentHasSpawnTool(parent) {
		return nil
	}

	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		if id == parentID {
			continue
		}
		if !agentAllowsSubagent(parent, id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	descriptors := make([]AgentDescriptor, 0, len(ids))
	for _, id := range ids {
		agent := r.agents[id]
		if agent == nil {
			continue
		}
		descriptors = append(descriptors, r.buildAgentDescriptorLocked(agent))
	}
	return descriptors
}

// GetAgentDescriptor 返回单个 agent 的结构化发现载荷。
func (r *AgentRegistry) GetAgentDescriptor(agentID string) (*AgentDescriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id := routing.NormalizeAgentID(agentID)
	agent, ok := r.agents[id]
	if !ok || agent == nil {
		return nil, false
	}

	descriptor := r.buildAgentDescriptorLocked(agent)
	return &descriptor, true
}

func (r *AgentRegistry) buildAgentDescriptorLocked(agent *AgentInstance) AgentDescriptor {
	definition := loadAgentDefinition(agent.Workspace)
	name, description := descriptorIdentity(agent.ID, definition)

	return AgentDescriptor{
		ID:          agent.ID,
		Name:        name,
		Description: description,
	}
}

func descriptorIdentity(agentID string, definition AgentContextDefinition) (string, string) {
	name := agentID
	description := ""
	if definition.Agent != nil {
		if trimmed := strings.TrimSpace(definition.Agent.Frontmatter.Name); trimmed != "" {
			name = trimmed
		}
		if trimmed := strings.TrimSpace(definition.Agent.Frontmatter.Description); trimmed != "" {
			description = trimmed
		}
	}

	if description == "" &&
		definition.Agent != nil {
		if definition.Source == AgentDefinitionSourceAgent {
			description = firstNonEmptyLine(definition.Agent.Body)
		} else if definition.Source == AgentDefinitionSourceAgents {
			description = firstMeaningfulParagraph(definition.Agent.Body)
		}
	}

	return name, description
}

func firstNonEmptyLine(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstMeaningfulParagraph(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	paragraphs := strings.Split(content, "\n\n")
	for _, paragraph := range paragraphs {
		lines := strings.Split(paragraph, "\n")
		parts := make([]string, 0, len(lines))
		inFence := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inFence = !inFence
				continue
			}
			if inFence || trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				trimmed = strings.TrimSpace(trimmed[2:])
			}
			parts = append(parts, trimmed)
		}
		if len(parts) == 0 {
			continue
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func (r *AgentRegistry) workspaceForAgentIDLocked(agentID string) string {
	agent, ok := r.agents[routing.NormalizeAgentID(agentID)]
	if !ok || agent == nil {
		return ""
	}
	return agent.Workspace
}

func (r *AgentRegistry) defaultAgentIDLocked() string {
	if _, ok := r.agents[routing.DefaultAgentID]; ok {
		return routing.DefaultAgentID
	}
	if r.cfg != nil && len(r.cfg.Agents.List) > 0 {
		for _, agentCfg := range r.cfg.Agents.List {
			if !agentCfg.Default {
				continue
			}
			id := routing.NormalizeAgentID(agentCfg.ID)
			if _, ok := r.agents[id]; ok {
				return id
			}
		}
		id := routing.NormalizeAgentID(r.cfg.Agents.List[0].ID)
		if _, ok := r.agents[id]; ok {
			return id
		}
	}
	for id := range r.agents {
		return id
	}
	return ""
}

func cleanWorkspacePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func formatAgentDiscoverySection(agents []AgentDescriptor) string {
	if len(agents) == 0 {
		return ""
	}

	payload := struct {
		Agents []AgentDescriptor `json:"agents"`
	}{
		Agents: agents,
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}

	var header strings.Builder
	header.WriteString("# Agent 发现\n\n")
	header.WriteString("此注册表列出当前 agent 被允许 spawn 的对等 agent。\n")
	header.WriteString(
		"按描述选择对等 agent；调用 spawn 仅可使用下列 agent ID。\n\n",
	)
	header.WriteString("```json\n")
	header.Write(encoded)
	header.WriteString("\n```")

	return header.String()
}
