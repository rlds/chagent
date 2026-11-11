package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/agent/workspacecontext"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/isolation"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/memory"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/routing"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/tools"
	dshfssearch "github.com/rlds/chagent/pkg/tools/dsh/fssearch"
	dshgoal "github.com/rlds/chagent/pkg/tools/dsh/goal"
	dshjobs "github.com/rlds/chagent/pkg/tools/dsh/jobs"
	dshsessionquery "github.com/rlds/chagent/pkg/tools/dsh/sessionquery"
	integrationtools "github.com/rlds/chagent/pkg/tools/integration"
)

// AgentInstance 表示一个完整配置的 agent，拥有自己的工作区、
// 会话管理器、上下文构建器和工具注册表。
type AgentInstance struct {
	ID                        string
	Name                      string
	Model                     string
	Fallbacks                 []string
	Workspace                 string
	MaxIterations             int
	MaxTokens                 int
	Temperature               float64
	ThinkingLevel             ThinkingLevel
	ThinkingLevelConfigured   bool
	ContextWindow             int
	SummarizeMessageThreshold int
	SummarizeTokenPercent     int
	Provider                  providers.LLMProvider
	Sessions                  session.SessionStore
	ContextBuilder            *ContextBuilder
	Tools                     *tools.ToolRegistry
	WorkspaceContext          *workspacecontext.Projector
	RuntimeContextMode        string
	RuntimeContextInterval    time.Duration
	RuntimeContextSender      bool
	Definition                AgentContextDefinition
	Subagents                 *config.SubagentsConfig
	SkillsFilter              []string
	MCPServerAllowlist        map[string]struct{}
	Candidates                []providers.FallbackCandidate
	ImageCandidates           []providers.FallbackCandidate

	// Router 在配置了模型路由且轻量模型成功解析时非 nil。
	// 它为每条传入消息打分，并决定是路由到 LightCandidates
	// 还是继续使用 Candidates。
	Router *routing.Router
	// LightCandidates 保存为轻量模型解析出的 provider 候选。
	// 在创建 agent 时预先计算，避免运行时重复查找 model_list。
	LightCandidates []providers.FallbackCandidate
	// LightProvider 是所配置轻量模型对应的具体 provider 实例。
	// 仅在路由为该回合选择轻量层级时使用。
	LightProvider providers.LLMProvider
	// CandidateProviders 将 "provider/model" 键映射到各候选的 LLMProvider
	// 实例。这让每个回退模型都能使用自己在 model_list 中的 api_base 和 api_key，
	// 而不是继承主模型的 provider 配置。
	CandidateProviders map[string]providers.LLMProvider

	// DSHJobs 拥有由独立 DSH 工具组启动的后台任务。
	// 除非 jobs 子组已启用，否则为 nil；Close() 会终止它们。
	DSHJobs *dshjobs.Manager
}

// NewAgentInstance 根据配置创建 agent 实例。
func NewAgentInstance(
	agentCfg *config.AgentConfig,
	defaults *config.AgentDefaults,
	cfg *config.Config,
	provider providers.LLMProvider,
) *AgentInstance {
	if cfg != nil {
		// 在工具或 provider 开始派生任何子进程之前，
		// 让子进程隔离运行时与最新加载的配置保持一致。
		isolation.Configure(cfg)
	}

	workspace := resolveAgentWorkspace(agentCfg, defaults)
	os.MkdirAll(workspace, 0o755)

	definition := loadAgentDefinition(workspace)

	model := resolveAgentModel(agentCfg, defaults, definition)
	fallbacks := resolveAgentFallbacks(agentCfg, defaults)

	restrict := defaults.RestrictToWorkspace
	readRestrict := restrict && !defaults.AllowReadOutsideWorkspace

	// 从配置编译路径白名单模式。
	allowReadPaths := buildAllowReadPatterns(cfg)
	allowWritePaths := compilePatterns(cfg.Tools.AllowWritePaths)
	agentToolAllowlist := resolveAgentToolAllowlist(definition)
	agentMCPServerAllowlist := resolveAgentMCPServerAllowlist(definition)

	toolsRegistry := tools.NewToolRegistry()
	toolsRegistry.SetAllowlist(agentToolAllowlist)

	if cfg.Tools.IsToolEnabled("read_file") {
		maxReadFileSize := cfg.Tools.ReadFile.MaxReadFileSize
		switch cfg.Tools.ReadFile.EffectiveMode() {
		case config.ReadFileModeLines:
			toolsRegistry.Register(tools.NewReadFileLinesTool(workspace, readRestrict, maxReadFileSize, allowReadPaths))
		default:
			toolsRegistry.Register(tools.NewReadFileBytesTool(workspace, readRestrict, maxReadFileSize, allowReadPaths))
		}
	}
	if cfg.Tools.IsToolEnabled("write_file") {
		toolsRegistry.Register(tools.NewWriteFileTool(workspace, restrict, allowWritePaths))
	}
	if cfg.Tools.IsToolEnabled("list_dir") {
		toolsRegistry.Register(tools.NewListDirTool(workspace, readRestrict, allowReadPaths))
	}
	if cfg.Tools.IsToolEnabled("exec") {
		execTool, err := tools.NewExecToolWithConfig(workspace, restrict, cfg, allowReadPaths)
		if err != nil {
			logger.ErrorCF("agent", "Failed to initialize exec tool; continuing without exec",
				map[string]any{"error": err.Error()})
		} else {
			toolsRegistry.Register(execTool)
		}
	}

	if cfg.Tools.IsToolEnabled("edit_file") {
		toolsRegistry.Register(tools.NewEditFileTool(workspace, restrict, allowWritePaths))
	}
	if cfg.Tools.IsToolEnabled("append_file") {
		toolsRegistry.Register(tools.NewAppendFileTool(workspace, restrict, allowWritePaths))
	}

	// 独立 DSH 工具组：这些工具与现有 Chagent 工具的功能并不等同，
	// 从 DSH 移植而来并放在专属分组中（PromptMetadata slot "dsh"）。
	// 该分组默认启用，并可通过 tools.dsh.* 按子组调整。
	var dshJobsManager *dshjobs.Manager
	if cfg.Tools.DSH.IsGlobEnabled() {
		toolsRegistry.Register(dshfssearch.NewGlobTool(workspace, readRestrict, allowReadPaths))
	}
	if cfg.Tools.DSH.IsGrepEnabled() {
		toolsRegistry.Register(dshfssearch.NewGrepTool(workspace, readRestrict, allowReadPaths))
	}
	if cfg.Tools.DSH.IsSessionQueryEnabled() {
		toolsRegistry.Register(dshsessionquery.NewSearchTool(workspace))
		toolsRegistry.Register(dshsessionquery.NewTraceTool(workspace))
	}
	if cfg.Tools.DSH.IsGoalEnabled() {
		goalManager := dshgoal.NewManager(workspace)
		dshgoal.Register(toolsRegistry, goalManager)
	}
	if cfg.Tools.DSH.IsJobsEnabled() {
		dshJobsManager = dshjobs.NewManager(workspace, dshjobs.Options{})
		dshjobs.Register(toolsRegistry, dshJobsManager)
	}

	sessionsDir := filepath.Join(workspace, "sessions")
	sessions := initSessionStore(sessionsDir)

	// Chess 任务/自主控制工具。TaskState 按会话隔离且持久；
	// 此时会话已经存在，因此在这里注册这些工具。
	taskOpts := integrationtools.TaskToolOptions{
		Sessions:                sessions,
		BlockedThreshold:        defaults.Autonomy.NoProgressRounds,
		DefaultMaxRounds:        defaults.Autonomy.MaxRounds,
		AllowParallelInProgress: defaults.Autonomy.AllowParallelInProgress,
	}
	if cfg.Tools.IsToolEnabled("task") {
		toolsRegistry.Register(integrationtools.NewTaskGetTool(taskOpts))
		toolsRegistry.Register(integrationtools.NewTaskUpdateTool(taskOpts))
	}
	if cfg.Tools.IsToolEnabled("todo") {
		toolsRegistry.Register(integrationtools.NewTodoWriteTool(taskOpts))
	}

	mcpDiscoveryActive := agentHasDiscoverableMCPServers(cfg, agentMCPServerAllowlist)
	// T4.3：渐进披露技能目录。context 投递要求 `skill` 工具加载正文；
	// 工具被禁用时回退到遗留 system prompt 目录，
	// 让模型仍能看到已安装内容。
	skillCatalogMode := cfg.Tools.Skills.EffectiveCatalogMode()
	if skillCatalogMode == config.SkillsCatalogModeContext && !cfg.Tools.IsToolEnabled("skill_load") {
		skillCatalogMode = config.SkillsCatalogModeSystem
	}
	contextBuilder := NewContextBuilder(workspace).
		WithToolDiscovery(
			mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseBM25,
			mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseRegex,
		).
		WithSplitOnMarker(cfg.Agents.Defaults.SplitOnMarker).
		WithRuntimeContextMode(defaults.RuntimeContext.Mode).
		WithPolicyPrompts(defaults.Autonomy.Enabled, defaults.WorkspaceContext.Enabled).
		WithSkillCatalogMode(skillCatalogMode)

	if cfg.Tools.IsToolEnabled("skill_load") {
		if loader := contextBuilder.SkillLoader(); loader != nil {
			toolsRegistry.Register(integrationtools.NewSkillLoadTool(loader))
		}
	}

	var workspaceContext *workspacecontext.Projector
	if defaults.WorkspaceContext.Enabled {
		wsCfg := workspacecontext.Config{
			Enabled:               true,
			MaxBytes:              defaults.WorkspaceContext.MaxBytes,
			MaxSourceBytes:        defaults.WorkspaceContext.MaxSourceBytes,
			ProjectRootMarkers:    append([]string(nil), defaults.WorkspaceContext.ProjectRootMarkers...),
			InstructionFiles:      append([]string(nil), defaults.WorkspaceContext.InstructionFiles...),
			LocalInstructionFiles: append([]string(nil), defaults.WorkspaceContext.LocalInstructionFiles...),
			UserGlobal:            defaults.WorkspaceContext.UserGlobal,
			FollowSymlinks:        defaults.WorkspaceContext.FollowSymlinks,
			ChagentHome:           config.GetHome(),
		}
		if home, err := os.UserHomeDir(); err == nil {
			wsCfg.AgentsHome = filepath.Join(home, ".agents")
		}
		excludePath := ""
		if definition.Agent != nil {
			excludePath = definition.Agent.Path
		}
		projector, err := workspacecontext.NewProjector(wsCfg, workspace, excludePath)
		if err != nil {
			logger.WarnCF("agent", "workspace context disabled: projector init failed", map[string]any{
				"error": err.Error(),
			})
		} else {
			workspaceContext = projector
		}
	}

	agentID := routing.DefaultAgentID
	agentName := ""
	var subagents *config.SubagentsConfig
	var skillsFilter []string

	if agentCfg != nil {
		agentID = routing.NormalizeAgentID(agentCfg.ID)
		agentName = agentCfg.Name
		if definition.Agent != nil && strings.TrimSpace(definition.Agent.Frontmatter.Name) != "" {
			agentName = strings.TrimSpace(definition.Agent.Frontmatter.Name)
		}
		subagents = agentCfg.Subagents
		skillsFilter = resolveAgentSkillsFilter(agentCfg, definition)
	}
	provider = resolvePrimaryProviderForAgent(cfg, workspace, agentID, model, provider)
	warnOnUnknownAgentMCPServerDeclarations(agentID, workspace, cfg, definition)

	maxIter := defaults.MaxToolIterations
	if maxIter == 0 {
		maxIter = 20
	} else if maxIter < 0 {
		// 负值会让循环条件立即为假，从而不调用模型就直接返回预设响应；
		// 因此钳制为一次迭代，让配置的上限控制该回合。
		logger.WarnCF("agent", "Negative max_tool_iterations clamped", map[string]any{
			"configured": defaults.MaxToolIterations,
		})
		maxIter = 1
	}

	maxTokens := defaults.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}

	contextWindow := defaults.ContextWindow
	if contextWindow == 0 {
		// 默认启发式：输出 token 上限的 4 倍。
		// 大多数模型的上下文窗口远高于其输出上限
		// （例如 GPT-4o 128k ctx / 16k out，Claude 200k ctx / 8k out）。
		// 4 倍是保守的下界，既能避免过早摘要，又仍然安全 ——
		// 响应式的 forceCompression 会处理任何超出。
		contextWindow = maxTokens * 4
	}

	temperature := 0.7
	if defaults.Temperature != nil {
		temperature = *defaults.Temperature
	}

	var thinkingLevelStr string
	if mc, err := cfg.GetModelConfig(model); err == nil {
		thinkingLevelStr = mc.ThinkingLevel
	}
	thinkingLevel := parseThinkingLevel(thinkingLevelStr)
	thinkingLevelConfigured := isConfiguredThinkingLevel(thinkingLevelStr)

	summarizeMessageThreshold := defaults.SummarizeMessageThreshold
	if summarizeMessageThreshold == 0 {
		summarizeMessageThreshold = 20
	}

	summarizeTokenPercent := defaults.SummarizeTokenPercent
	if summarizeTokenPercent == 0 {
		summarizeTokenPercent = 75
	}

	// 解析回退候选。
	candidates := resolveModelCandidates(cfg, defaults.Provider, model, fallbacks)
	imageCandidates := resolveModelCandidates(
		cfg,
		defaults.Provider,
		defaults.ImageModel,
		defaults.ImageModelFallbacks,
	)

	candidateProviders := make(map[string]providers.LLMProvider)
	populateCandidateProvidersFromNames(cfg, workspace, fallbacks, candidateProviders)
	if strings.TrimSpace(defaults.ImageModel) != "" {
		imageNames := append([]string{defaults.ImageModel}, defaults.ImageModelFallbacks...)
		populateCandidateProvidersFromNames(cfg, workspace, imageNames, candidateProviders)
	}

	// 模型路由设置：在创建时预先解析轻量模型候选，
	// 避免对每条传入消息重复查找 model_list。
	var router *routing.Router
	var lightCandidates []providers.FallbackCandidate
	var lightProvider providers.LLMProvider
	if rc := defaults.Routing; rc != nil && rc.Enabled && rc.LightModel != "" {
		resolved := resolveModelCandidates(cfg, defaults.Provider, rc.LightModel, nil)
		if len(resolved) > 0 {
			lightModelCfg, err := resolvedModelConfig(cfg, rc.LightModel, workspace)
			if err != nil {
				logger.WarnCF(
					"agent",
					"Routing light model config invalid; routing disabled",
					map[string]any{
						"light_model": rc.LightModel,
						"agent_id":    agentID,
						"error":       err.Error(),
					},
				)
			} else {
				lp, _, err := providers.CreateProviderFromConfig(lightModelCfg)
				if err != nil {
					logger.WarnCF("agent", "Routing light model provider init failed; routing disabled",
						map[string]any{"light_model": rc.LightModel, "agent_id": agentID, "error": err.Error()})
				} else {
					router = routing.New(routing.RouterConfig{
						LightModel: rc.LightModel,
						Threshold:  rc.Threshold,
					})
					lightCandidates = resolved
					lightProvider = lp
					populateCandidateProvidersFromNames(cfg, workspace, []string{rc.LightModel}, candidateProviders)
				}
			}
		} else {
			logger.WarnCF("agent", "Routing light model not found; routing disabled",
				map[string]any{"light_model": rc.LightModel, "agent_id": agentID})
		}
	}

	return &AgentInstance{
		ID:                        agentID,
		Name:                      agentName,
		Model:                     model,
		Fallbacks:                 fallbacks,
		Workspace:                 workspace,
		MaxIterations:             maxIter,
		MaxTokens:                 maxTokens,
		Temperature:               temperature,
		ThinkingLevel:             thinkingLevel,
		ThinkingLevelConfigured:   thinkingLevelConfigured,
		ContextWindow:             contextWindow,
		SummarizeMessageThreshold: summarizeMessageThreshold,
		SummarizeTokenPercent:     summarizeTokenPercent,
		Provider:                  provider,
		Sessions:                  sessions,
		ContextBuilder:            contextBuilder,
		Tools:                     toolsRegistry,
		DSHJobs:                   dshJobsManager,
		WorkspaceContext:          workspaceContext,
		RuntimeContextMode:        defaults.RuntimeContext.Mode,
		RuntimeContextInterval:    time.Duration(defaults.RuntimeContext.RefreshIntervalSeconds) * time.Second,
		RuntimeContextSender:      defaults.RuntimeContext.IncludeSender,
		Definition:                definition,
		Subagents:                 subagents,
		SkillsFilter:              skillsFilter,
		MCPServerAllowlist:        agentMCPServerAllowlist,
		Candidates:                candidates,
		ImageCandidates:           imageCandidates,
		Router:                    router,
		LightCandidates:           lightCandidates,
		LightProvider:             lightProvider,
		CandidateProviders:        candidateProviders,
	}
}

// populateCandidateProvidersFromNames 通过 resolvedModelConfig 解析每个模型名
// （别名或 "provider/model"），并为其创建专用的 LLMProvider。
// 它复用规范的配置解析路径（GetModelConfig），
// 使别名处理与负载均衡与代码库其余部分保持一致。
func populateCandidateProvidersFromNames(
	cfg *config.Config,
	workspace string,
	names []string,
	out map[string]providers.LLMProvider,
) {
	if cfg == nil || len(names) == 0 {
		return
	}
	for _, name := range names {
		mc, err := resolvedModelConfig(cfg, strings.TrimSpace(name), workspace)
		if err != nil {
			logger.WarnCF("agent",
				"fallback provider: no model_list entry found; will inherit primary provider credentials",
				map[string]any{"name": name, "error": err.Error()})
			continue
		}
		protocol, modelID := providers.ExtractProtocol(mc)
		key := providers.ModelKey(protocol, modelID)
		if _, exists := out[key]; exists {
			continue
		}
		p, _, err := providers.CreateProviderFromConfig(mc)
		if err != nil {
			logger.WarnCF("agent", "fallback provider: failed to create provider",
				map[string]any{"model": mc.Model, "error": err.Error()})
			continue
		}
		out[key] = p
	}
}

// resolvePrimaryProviderForAgent 在活动主模型指向某个 model_list 条目时，
// 为该模型解析专用 provider。这让 agent 的单候选路径与所选模型自身的
// provider/api_base/api_key 保持一致，而不是继承进程默认 provider。
func resolvePrimaryProviderForAgent(
	cfg *config.Config,
	workspace string,
	agentID string,
	model string,
	fallback providers.LLMProvider,
) providers.LLMProvider {
	model = strings.TrimSpace(model)
	if cfg == nil || model == "" {
		return fallback
	}

	modelCfg := lookupModelConfigByRef(cfg, model)
	if modelCfg == nil {
		return fallback
	}
	clone := *modelCfg
	if clone.Workspace == "" {
		clone.Workspace = workspace
	}

	resolvedProvider, _, err := providers.CreateProviderFromConfig(&clone)
	if err != nil {
		logger.WarnCF("agent", "Primary model provider init failed; using injected provider",
			map[string]any{
				"agent_id": agentID,
				"model":    model,
				"error":    err.Error(),
			})
		return fallback
	}
	if resolvedProvider == nil {
		return fallback
	}
	return resolvedProvider
}

// resolveAgentWorkspace 确定 agent 的工作区目录。
func resolveAgentWorkspace(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
		return expandHome(strings.TrimSpace(agentCfg.Workspace))
	}
	// 使用配置的默认工作区（遵循 CHAGENT_HOME）
	if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" ||
		routing.NormalizeAgentID(agentCfg.ID) == "main" {
		return expandHome(defaults.Workspace)
	}
	// 对于没有显式工作区的命名 agent，使用默认工作区并附加 agent ID 后缀
	id := routing.NormalizeAgentID(agentCfg.ID)
	return filepath.Join(expandHome(defaults.Workspace), "..", "workspace-"+id)
}

// resolveAgentModel 解析 agent 的主模型。
func resolveAgentModel(
	agentCfg *config.AgentConfig,
	defaults *config.AgentDefaults,
	definition AgentContextDefinition,
) string {
	if definition.Agent != nil && strings.TrimSpace(definition.Agent.Frontmatter.Model) != "" {
		return strings.TrimSpace(definition.Agent.Frontmatter.Model)
	}
	if agentCfg != nil && agentCfg.Model != nil && strings.TrimSpace(agentCfg.Model.Primary) != "" {
		return strings.TrimSpace(agentCfg.Model.Primary)
	}
	return defaults.GetModelName()
}

// resolveAgentFallbacks 解析 agent 的回退模型。
func resolveAgentFallbacks(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) []string {
	if agentCfg != nil && agentCfg.Model != nil && agentCfg.Model.Fallbacks != nil {
		return agentCfg.Model.Fallbacks
	}
	return defaults.ModelFallbacks
}

func resolveAgentSkillsFilter(
	agentCfg *config.AgentConfig,
	definition AgentContextDefinition,
) []string {
	if definition.Agent != nil && definition.Agent.Frontmatter.Skills != nil {
		return append([]string(nil), definition.Agent.Frontmatter.Skills...)
	}
	if agentCfg == nil || agentCfg.Skills == nil {
		return nil
	}
	return append([]string(nil), agentCfg.Skills...)
}

func (a *AgentInstance) AllowsMCPServer(serverName string) bool {
	if a == nil || a.MCPServerAllowlist == nil {
		return true
	}
	_, ok := a.MCPServerAllowlist[strings.ToLower(strings.TrimSpace(serverName))]
	return ok
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			logger.WarnCF("agent", "invalid path pattern in compilePatterns", map[string]any{
				"pattern": p,
				"error":   err.Error(),
			})
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled
}

func buildAllowReadPatterns(cfg *config.Config) []*regexp.Regexp {
	var configured []string
	if cfg != nil {
		configured = cfg.Tools.AllowReadPaths
	}

	compiled := compilePatterns(configured)
	mediaDirPattern := regexp.MustCompile(mediaTempDirPattern())
	for _, pattern := range compiled {
		if pattern.String() == mediaDirPattern.String() {
			return compiled
		}
	}

	return append(compiled, mediaDirPattern)
}

func mediaTempDirPattern() string {
	sep := regexp.QuoteMeta(string(os.PathSeparator))
	return "^" + regexp.QuoteMeta(filepath.Clean(media.TempDir())) + "(?:" + sep + "|$)"
}

// Close 释放 agent 持有的资源：先释放后台 DSH 任务，
// 再释放会话存储。
func (a *AgentInstance) Close() error {
	var firstErr error
	if a.DSHJobs != nil {
		if err := a.DSHJobs.Close(); err != nil {
			firstErr = err
		}
		a.DSHJobs = nil
	}
	if a.Sessions != nil {
		if err := a.Sessions.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// initSessionStore 创建会话持久化后端。
// 默认使用 JSONL 存储，并自动迁移遗留的 JSON 会话。
// 如果 JSONL 存储无法初始化，或迁移失败（说明该存储无法可靠写入），
// 则回退到 SessionManager。
func initSessionStore(dir string) session.SessionStore {
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		logger.WarnCF("agent", "Memory JSONL store init failed; falling back to json sessions",
			map[string]any{"error": err.Error()})
		return session.NewSessionManager(dir)
	}

	if n, merr := memory.MigrateFromJSON(context.Background(), dir, store); merr != nil {
		// 迁移失败意味着该存储无法写入数据。
		// 回退到 SessionManager，避免出现部分会话在 JSONL 中、
		// 另一些仍留在 JSON 中的分裂状态。
		logger.WarnCF("agent", "Memory migration failed; falling back to json sessions",
			map[string]any{"error": merr.Error()})
		store.Close()
		return session.NewSessionManager(dir)
	} else if n > 0 {
		logger.InfoCF("agent", "Memory migrated to JSONL", map[string]any{"sessions_migrated": n})
	}

	return session.NewJSONLBackend(store)
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}
