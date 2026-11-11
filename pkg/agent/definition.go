package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gomarkdown/markdown/parser"
	"gopkg.in/yaml.v3"

	"github.com/rlds/chagent/pkg/logger"
)

// AgentDefinitionSource 标识某个定义是由哪个 agent 引导文件产生的。
type AgentDefinitionSource string

const (
	// AgentDefinitionSourceAgent 表示新的 AGENT.md 格式。
	AgentDefinitionSourceAgent AgentDefinitionSource = "AGENT.md"
	// AgentDefinitionSourceAgents 表示旧版的 AGENTS.md 格式。
	AgentDefinitionSourceAgents AgentDefinitionSource = "AGENTS.md"
)

// AgentFrontmatter 保存 AGENT.md 中机器可读的配置。
//
// 已知字段为方便使用而直接暴露。Fields 保留完整的已解析 frontmatter，
// 以便将来的重构可以读取额外的键，而无需再次改动加载器的契约。
type AgentFrontmatter struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Tools       []string       `json:"tools,omitempty"`
	Model       string         `json:"model,omitempty"`
	MaxTurns    *int           `json:"maxTurns,omitempty"`
	Skills      []string       `json:"skills,omitempty"`
	MCPServers  []string       `json:"mcpServers,omitempty"`
	Fields      map[string]any `json:"-"`
}

// AgentPromptDefinition 表示已解析的 AGENT.md 或 AGENTS.md prompt 文件。
type AgentPromptDefinition struct {
	Path           string           `json:"path"`
	Raw            string           `json:"raw"`
	Body           string           `json:"body"`
	RawFrontmatter string           `json:"raw_frontmatter,omitempty"`
	Frontmatter    AgentFrontmatter `json:"frontmatter"`
	FrontmatterErr string           `json:"frontmatter_error,omitempty"`
}

// SoulDefinition 表示与 agent 关联的、已解析的 SOUL.md 文件。
type SoulDefinition struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// UserDefinition 表示与工作区关联的、已解析的 USER.md 文件。
type UserDefinition struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// AgentContextDefinition 以运行时友好的结构承载工作区的 agent 定义。
type AgentContextDefinition struct {
	Source AgentDefinitionSource  `json:"source,omitempty"`
	Agent  *AgentPromptDefinition `json:"agent,omitempty"`
	Soul   *SoulDefinition        `json:"soul,omitempty"`
	User   *UserDefinition        `json:"user,omitempty"`
}

// LoadAgentDefinition 解析工作区的 agent 引导文件。
//
// 它优先采用新的 AGENT.md 格式及其配套的 SOUL.md 文件。当结构化文件缺失时，
// 则回退到旧版 AGENTS.md 布局，使当前运行时能够逐步迁移。
func (cb *ContextBuilder) LoadAgentDefinition() AgentContextDefinition {
	return loadAgentDefinition(cb.workspace)
}

func loadAgentDefinition(workspace string) AgentContextDefinition {
	definition := AgentContextDefinition{}
	definition.User = loadUserDefinition(workspace)
	agentPath := filepath.Join(workspace, string(AgentDefinitionSourceAgent))
	if content, err := os.ReadFile(agentPath); err == nil {
		prompt := parseAgentPromptDefinition(agentPath, string(content))
		definition.Source = AgentDefinitionSourceAgent
		definition.Agent = &prompt
		soulPath := filepath.Join(workspace, "SOUL.md")
		if content, err := os.ReadFile(soulPath); err == nil {
			definition.Soul = &SoulDefinition{
				Path:    soulPath,
				Content: string(content),
			}
		}
		return definition
	}

	legacyPath := filepath.Join(workspace, string(AgentDefinitionSourceAgents))
	if content, err := os.ReadFile(legacyPath); err == nil {
		definition.Source = AgentDefinitionSourceAgents
		definition.Agent = &AgentPromptDefinition{
			Path: legacyPath,
			Raw:  string(content),
			Body: string(content),
		}
	}

	defaultSoulPath := filepath.Join(workspace, "SOUL.md")
	if definition.Source != "" || fileExists(defaultSoulPath) {
		if content, err := os.ReadFile(defaultSoulPath); err == nil {
			definition.Soul = &SoulDefinition{
				Path:    defaultSoulPath,
				Content: string(content),
			}
		}
	}

	return definition
}

func (definition AgentContextDefinition) trackedPaths(workspace string) []string {
	paths := []string{
		filepath.Join(workspace, string(AgentDefinitionSourceAgent)),
		filepath.Join(workspace, "SOUL.md"),
		filepath.Join(workspace, "USER.md"),
	}
	if definition.Source != AgentDefinitionSourceAgent {
		paths = append(paths,
			filepath.Join(workspace, string(AgentDefinitionSourceAgents)),
			filepath.Join(workspace, "IDENTITY.md"),
		)
	}
	return uniquePaths(paths)
}

func loadUserDefinition(workspace string) *UserDefinition {
	userPath := filepath.Join(workspace, "USER.md")
	if content, err := os.ReadFile(userPath); err == nil {
		return &UserDefinition{
			Path:    userPath,
			Content: string(content),
		}
	}

	return nil
}

func parseAgentPromptDefinition(path, content string) AgentPromptDefinition {
	frontmatter, body := splitAgentFrontmatter(content)
	parsedFrontmatter, err := parseAgentFrontmatter(path, frontmatter)
	return AgentPromptDefinition{
		Path:           path,
		Raw:            content,
		Body:           body,
		RawFrontmatter: frontmatter,
		Frontmatter:    parsedFrontmatter,
		FrontmatterErr: errorString(err),
	}
}

func parseAgentFrontmatter(path, frontmatter string) (AgentFrontmatter, error) {
	frontmatter = strings.TrimSpace(frontmatter)
	if frontmatter == "" {
		return AgentFrontmatter{}, nil
	}

	rawFields := make(map[string]any)
	if err := yaml.Unmarshal([]byte(frontmatter), &rawFields); err != nil {
		logger.WarnCF("agent", "Failed to parse AGENT.md frontmatter", map[string]any{
			"path":  path,
			"error": err.Error(),
		})
		return AgentFrontmatter{}, err
	}

	var typed struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Tools       []string `yaml:"tools"`
		Model       string   `yaml:"model"`
		MaxTurns    *int     `yaml:"maxTurns"`
		Skills      []string `yaml:"skills"`
		MCPServers  []string `yaml:"mcpServers"`
	}
	if err := yaml.Unmarshal([]byte(frontmatter), &typed); err != nil {
		logger.WarnCF("agent", "Failed to decode AGENT.md frontmatter fields", map[string]any{
			"path":  path,
			"error": err.Error(),
		})
		return AgentFrontmatter{}, err
	}

	return AgentFrontmatter{
		Name:        strings.TrimSpace(typed.Name),
		Description: strings.TrimSpace(typed.Description),
		Tools:       append([]string(nil), typed.Tools...),
		Model:       strings.TrimSpace(typed.Model),
		MaxTurns:    typed.MaxTurns,
		Skills:      append([]string(nil), typed.Skills...),
		MCPServers:  append([]string(nil), typed.MCPServers...),
		Fields:      rawFields,
	}, nil
}

func splitAgentFrontmatter(content string) (frontmatter, body string) {
	normalized := string(parser.NormalizeNewlines([]byte(content)))
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", content
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", content
	}

	frontmatter = strings.Join(lines[1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	body = strings.TrimLeft(body, "\n")
	return frontmatter, body
}

func relativeWorkspacePath(workspace, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	relativePath, err := filepath.Rel(workspace, path)
	if err == nil && relativePath != "." && !strings.HasPrefix(relativePath, "..") {
		return filepath.ToSlash(relativePath)
	}
	return filepath.Clean(path)
}

func uniquePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		cleaned := filepath.Clean(path)
		if slices.Contains(result, cleaned) {
			continue
		}
		result = append(result, cleaned)
	}
	return result
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
