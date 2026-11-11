// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/commands"
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/skills"
	"github.com/rlds/chagent/pkg/state"
	"github.com/rlds/chagent/pkg/tools"
	dshsubagentctl "github.com/rlds/chagent/pkg/tools/dsh/subagentctl"
)

func NewAgentLoop(
	cfg *config.Config,
	msgBus *bus.MessageBus,
	provider providers.LLMProvider,
	opts ...AgentLoopOption,
) *AgentLoop {
	registry := NewAgentRegistry(cfg, provider)

	// 设置带限流的共享回退链。
	cooldown := providers.NewCooldownTracker()
	rl := providers.NewRateLimiterRegistry()
	// 为所有 agent 的候选注册限流器，以便在每次 LLM 调用前强制执行
	// ModelConfig 中配置的 RPM 限制。
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			rl.RegisterCandidates(agent.Candidates)
			rl.RegisterCandidates(agent.LightCandidates)
		}
	}
	fallbackChain := providers.NewFallbackChain(cooldown, rl)

	// 使用默认 agent 的工作区创建状态管理器，用于记录频道信息
	defaultAgent := registry.GetDefaultAgent()
	var stateManager *state.Manager
	if defaultAgent != nil {
		stateManager = state.NewManager(defaultAgent.Workspace)
	}

	// 根据配置确定工作池大小（默认值 1，表示按顺序执行）
	workerPoolSize := cfg.Agents.Defaults.MaxParallelTurns
	if workerPoolSize <= 0 {
		workerPoolSize = 1
	}

	al := &AgentLoop{
		bus:               msgBus,
		cfg:               cfg,
		registry:          registry,
		state:             stateManager,
		fallback:          fallbackChain,
		cmdRegistry:       commands.NewRegistry(commands.BuiltinDefinitions()),
		steering:          newSteeringQueue(parseSteeringMode(cfg.Agents.Defaults.SteeringMode)),
		workerSem:         make(chan struct{}, workerPoolSize),
		ownsRuntimeEvents: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(al)
		}
	}
	if al.runtimeEvents == nil {
		al.runtimeEvents = runtimeevents.NewBus()
		al.ownsRuntimeEvents = true
	}
	al.activeReqCond = sync.NewCond(&al.activeReqMu)
	al.refreshRuntimeEventLogger(cfg)
	al.providerFactory = providers.CreateProviderFromConfig
	al.hooks = NewHookManager(al.runtimeEvents.Channel())
	configureHookManagerFromConfig(al.hooks, cfg)
	al.contextManager = al.resolveContextManager()

	// 向所有 agent 注册共享工具（此时 al 已创建完成）
	registerSharedTools(al, cfg, msgBus, registry, provider)

	return al
}

func registerSharedTools(
	al *AgentLoop,
	cfg *config.Config,
	msgBus interfaces.MessageBus,
	registry *AgentRegistry,
	provider providers.LLMProvider,
) {
	allowReadPaths := buildAllowReadPatterns(cfg)

	for _, agentID := range registry.ListAgentIDs() {
		agent, ok := registry.GetAgent(agentID)
		if !ok {
			continue
		}

		if cfg.Tools.IsToolEnabled("web") {
			searchTool, err := tools.NewWebSearchTool(tools.WebSearchToolOptionsFromConfig(cfg))
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web search tool", map[string]any{"error": err.Error()})
			} else if searchTool != nil {
				agent.Tools.Register(searchTool)
			}
		}
		if cfg.Tools.IsToolEnabled("web_fetch") {
			fetchTool, err := tools.NewWebFetchToolWithProxy(
				50000,
				cfg.Tools.Web.Proxy,
				cfg.Tools.Web.Format,
				cfg.Tools.Web.FetchLimitBytes,
				cfg.Tools.Web.PrivateHostWhitelist)
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
			} else {
				agent.Tools.Register(fetchTool)
			}
		}

		// 消息工具
		if cfg.Tools.IsToolEnabled("message") {
			messageTool := tools.NewMessageTool()
			if cfg.Tools.Message.MediaEnabled {
				messageTool.ConfigureLocalMedia(
					agent.Workspace,
					cfg.Agents.Defaults.RestrictToWorkspace,
					cfg.Agents.Defaults.GetMaxMediaSize(),
					allowReadPaths,
				)
			}
			messageTool.SetSendCallback(func(
				ctx context.Context,
				channel, chatID, content, replyToMessageID string,
				mediaParts []bus.MediaPart,
			) error {
				outboundCtx := bus.NewOutboundContext(channel, chatID, replyToMessageID)
				outboundAgentID, outboundSessionKey, outboundScope := outboundTurnMetadata(
					tools.ToolAgentID(ctx),
					tools.ToolSessionKey(ctx),
					tools.ToolSessionScope(ctx),
				)
				if len(mediaParts) > 0 {
					outboundMedia := bus.OutboundMediaMessage{
						Channel:    channel,
						ChatID:     chatID,
						Context:    outboundCtx,
						AgentID:    outboundAgentID,
						SessionKey: outboundSessionKey,
						Scope:      outboundScope,
						Parts:      mediaParts,
					}
					if al.channelManager != nil && channel != "" {
						return al.channelManager.SendMedia(ctx, outboundMedia)
					}
					pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer pubCancel()
					return msgBus.PublishOutboundMedia(pubCtx, outboundMedia)
				}
				outboundMessage := bus.OutboundMessage{
					Channel:          channel,
					ChatID:           chatID,
					Context:          outboundCtx,
					AgentID:          outboundAgentID,
					SessionKey:       outboundSessionKey,
					Scope:            outboundScope,
					Content:          content,
					ReplyToMessageID: replyToMessageID,
				}
				if al.channelManager != nil && channel != "" {
					return al.channelManager.SendMessage(ctx, outboundMessage)
				}
				pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer pubCancel()
				return msgBus.PublishOutbound(pubCtx, outboundMessage)
			})
			agent.Tools.Register(messageTool)
		}
		if cfg.Tools.IsToolEnabled("reaction") {
			reactionTool := tools.NewReactionTool()
			reactionTool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
				if al.channelManager == nil {
					return fmt.Errorf("channel manager not configured")
				}
				ch, ok := al.channelManager.GetChannel(channel)
				if !ok {
					return fmt.Errorf("channel %s not found", channel)
				}
				rc, ok := ch.(channels.ReactionCapable)
				if !ok {
					return fmt.Errorf("channel %s does not support reactions", channel)
				}
				_, err := rc.ReactToMessage(ctx, chatID, messageID)
				return err
			})
			agent.Tools.Register(reactionTool)
		}

		// 发送文件工具（通过 MediaStore 发送出站媒体 —— store 之后由 SetMediaStore 注入）
		if cfg.Tools.IsToolEnabled("send_file") {
			sendFileTool := tools.NewSendFileTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(sendFileTool)
		}

		if cfg.Tools.IsToolEnabled("load_image") {
			loadImageTool := tools.NewLoadImageTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(loadImageTool)
		}

		// 技能发现与安装工具
		skills_enabled := cfg.Tools.IsToolEnabled("skills")
		find_skills_enable := cfg.Tools.IsToolEnabled("find_skills")
		install_skills_enable := cfg.Tools.IsToolEnabled("install_skill")
		if skills_enabled && (find_skills_enable || install_skills_enable) {
			registryMgr := skills.NewRegistryManagerFromToolsConfig(cfg.Tools.Skills)

			if find_skills_enable {
				searchCache := skills.NewSearchCache(
					cfg.Tools.Skills.SearchCache.MaxSize,
					time.Duration(cfg.Tools.Skills.SearchCache.TTLSeconds)*time.Second,
				)
				agent.Tools.Register(tools.NewFindSkillsTool(registryMgr, searchCache))
			}

			if install_skills_enable {
				agent.Tools.Register(tools.NewInstallSkillTool(registryMgr, agent.Workspace))
			}
		}

		// spawn 与 spawn_status 工具共享同一个 SubagentManager。
		// 只要其中任一工具启用（两者都需要 subagent），就创建该管理器。
		spawnEnabled := cfg.Tools.IsToolEnabled("spawn")
		spawnStatusEnabled := cfg.Tools.IsToolEnabled("spawn_status")
		if (spawnEnabled || spawnStatusEnabled) && cfg.Tools.IsToolEnabled("subagent") {
			subagentManager := tools.NewSubagentManager(provider, agent.Model, agent.Workspace)
			subagentManager.SetLLMOptions(agent.MaxTokens, agent.Temperature)

			// 注入媒体解析器，使旧版 RunToolLoop 回退路径能以与主
			// AgentLoop 相同的方式解析 media:// 引用。
			// 这样即便优化后的子轮 spawner 路径不可用，子 agent 的
			// 视觉能力仍可正常工作。
			subagentManager.SetMediaResolver(func(msgs []providers.Message) []providers.Message {
				return resolveMediaRefs(msgs, al.mediaStore, cfg.Agents.Defaults.GetMaxMediaSize(), 0)
			})

			// 设置接入 AgentLoop turnState 的 spawner
			subagentManager.SetSpawner(func(
				ctx context.Context,
				task, label, targetAgentID string,
				tls *tools.ToolRegistry,
				maxTokens int,
				temperature float64,
				hasMaxTokens, hasTemperature bool,
			) (*tools.ToolResult, error) {
				// 1. 从 Context 中恢复父级 Turn State
				parentTS := turnStateFromContext(ctx)
				if parentTS == nil {
					// 回退：如果 Context 中不存在 turnState，则创建一个隔离的 ad-hoc 根 turn state，
					// 使该工具在 agent loop 之外（例如测试、原始调用）仍能正常工作。
					parentTS = &turnState{
						ctx:            ctx,
						turnID:         "adhoc-root",
						depth:          0,
						session:        nil, // adhoc 派生不需要临时会话
						pendingResults: make(chan *tools.ToolResult, 16),
						concurrencySem: make(chan struct{}, 5),
					}
				}

				// 2. 从注册表构建 Tools 切片
				var tlSlice []tools.Tool
				for _, name := range tls.List() {
					if t, ok := tls.Get(name); ok {
						tlSlice = append(tlSlice, t)
					}
				}

				// 3. 系统提示词
				systemPrompt := tools.SubagentSystemPrompt("", task, false)

				// 4. 解析模型
				modelToUse := agent.Model
				if targetAgentID != "" {
					if targetAgent, ok := al.GetRegistry().GetAgent(targetAgentID); ok {
						modelToUse = targetAgent.Model
					}
				}

				// 5. 构建 SubTurnConfig
				cfg := SubTurnConfig{
					Model:        modelToUse,
					Tools:        tlSlice,
					SystemPrompt: systemPrompt,
				}
				if hasMaxTokens {
					cfg.MaxTokens = maxTokens
				}

				// 6. 派生 SubTurn
				return spawnSubTurn(ctx, al, parentTS, cfg)
			})

			// 克隆父级的工具注册表，使子 agent 能使用目前注册的所有工具
			// （file、web 等），但不包含下面才添加的 spawn/spawn_status ——
			// 以避免出现递归派生 subagent 的情况。
			subagentManager.SetTools(agent.Tools.Clone())
			if spawnEnabled {
				spawnTool := tools.NewSpawnTool(subagentManager)
				spawnTool.SetSpawner(NewSubTurnSpawner(al))
				currentAgentID := agentID
				spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
					return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
				})

				agent.Tools.Register(spawnTool)

				// 同时注册同步版 subagent 工具
				subagentTool := tools.NewSubagentTool(subagentManager)
				subagentTool.SetSpawner(NewSubTurnSpawner(al))
				agent.Tools.Register(subagentTool)
			}
			if spawnStatusEnabled {
				agent.Tools.Register(tools.NewSpawnStatusTool(subagentManager))
			}
			// 独立的 DSH 工具组：在同一个管理器之上提供 subagent 控制面
			// （list/wait/interrupt）。仅在 subagent 运行时可用时才有意义。
			if cfg.Tools.DSH.IsSubagentControlEnabled() {
				dshsubagentctl.Register(agent.Tools, subagentManager)
			}
		} else if (spawnEnabled || spawnStatusEnabled) && !cfg.Tools.IsToolEnabled("subagent") {
			logger.WarnCF("agent", "spawn/spawn_status tools require subagent to be enabled", nil)
		}

		// 为多 agent 环境注册 delegate 工具。
		// 存在多个 agent 时自动启用。委派直接使用 SubTurn 机制
		// （而非 SubagentManager），与 subagent 工具相互独立。
		if len(registry.ListAgentIDs()) > 1 {
			delegateTool := tools.NewDelegateTool()
			delegateTool.SetSpawner(NewSubTurnSpawner(al))
			currentAgentID := agentID
			delegateTool.SetSelfAgentID(currentAgentID)
			delegateTool.SetAllowlistChecker(func(targetAgentID string) bool {
				return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
			})
			agent.Tools.Register(delegateTool)
		}

		warnOnUnknownAgentToolDeclarations(agentID, agent.Workspace, agent.Definition, agent.Tools)
	}
}
