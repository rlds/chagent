// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/constants"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/routing"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/state"
	"github.com/rlds/chagent/pkg/utils"
)

type AgentLoop struct {
	// 核心依赖
	bus      interfaces.MessageBus
	cfg      *config.Config
	registry *AgentRegistry
	state    *state.Manager

	// 运行时事件系统
	runtimeEvents      runtimeevents.Bus
	ownsRuntimeEvents  bool
	runtimeEventLogMu  sync.RWMutex
	runtimeEventLogger *runtimeEventLogger
	runtimeEventLogSub runtimeevents.Subscription
	hooks              *HookManager

	// 运行时状态
	running        atomic.Bool
	contextManager ContextManager
	fallback       *providers.FallbackChain
	channelManager interfaces.ChannelManager
	mediaStore     media.MediaStore
	cmdRegistry    *commands.Registry
	mcp            mcpRuntime
	hookRuntime    hookRuntime
	steering       *steeringQueue
	pendingSkills  sync.Map
	pendingStops   sync.Map
	// executionReports 是工具执行上报（execution.report）的异步发送器。
	// 默认开启；显式配置 execution_report.enabled=false 时不构建载荷、不发送。
	executionReports *executionReportSink
	mu               sync.RWMutex

	// workerSem 限制并发处理轮次的 worker 数量。
	workerSem chan struct{}

	// activeTurnStates 跟踪每个会话的活动轮次，防止重复。
	activeTurnStates sync.Map
	subTurnCounter   atomic.Int64

	// autonomy 驱动运行时状态：每个会话最多一个排队/延迟轮次，
	// 外加终态任务阶段唯一最终摘要的去重 map。
	autonomyMu          sync.Mutex
	autonomyPending     map[string]*pendingAutonomyRound
	autonomySummarySent map[string]string

	turnSeq atomic.Uint64

	// activeReqMu/activeReqCond/activeReqCount 替代 sync.WaitGroup，
	// 避免在 Add(1) 与由 goroutine 发起的 Wait() 竞争时出现
	// “WaitGroup is reused before previous Wait has returned” panic。
	activeReqMu    sync.Mutex
	activeReqCond  *sync.Cond
	activeReqCount int

	reloadFunc func() error

	providerFactory func(*config.ModelConfig) (providers.LLMProvider, string, error)
}

// processOptions 配置消息的处理方式
type processOptions struct {
	Dispatch                DispatchRequest // 本轮规范化后的路由请求边界
	SessionKey              string          // 用于历史/上下文的会话标识符
	SessionAliases          []string        // 会话键的兼容性别名
	Channel                 string          // 工具执行的目标 channel
	ChatID                  string          // 工具执行的目标 chat ID
	MessageID               string          // 当前入站平台消息 ID
	ReplyToMessageID        string          // 当前入站回复目标消息 ID
	SenderID                string          // 用于动态上下文的当前发送者 ID
	SenderDisplayName       string          // 用于动态上下文的当前发送者显示名
	UserMessage             string          // 用户消息内容（可能包含前缀）
	ForcedSkills            []string        // 本消息显式请求的技能
	TurnProfile             config.EffectiveTurnProfile
	SystemPromptOverride    string                 // 覆盖默认 system prompt（供子轮次使用）
	Media                   []string               // 来自入站消息的 media:// 引用
	InitialSteeringMessages []providers.Message    // 来自 refactor/agent 的引导消息
	DefaultResponse         string                 // LLM 返回空响应时使用的回复
	EnableSummary           bool                   // 是否触发摘要
	SendResponse            bool                   // 是否通过 bus 发送回复
	AllowInterimPicoPublish bool                   // 当 SendResponse 为 false 时，是否允许发布 pico 工具调用的中间文本
	SuppressToolFeedback    bool                   // 是否抑制内联工具反馈消息
	NoHistory               bool                   // 为 true 时不加载会话历史（用于心跳）
	SkipInitialSteeringPoll bool                   // 为 true 时跳过循环开始时的引导轮询（供 Continue 使用）
	InboundContext          *bus.InboundContext    // 供事件/钩子使用的规范化入站事实
	RouteResult             *routing.ResolvedRoute // 供事件/钩子使用的路由决策快照
	SessionScope            *session.SessionScope  // 供事件/钩子使用的会话范围快照
	TurnSource              string                 // human|scheduler|autonomy|subagent|system
}

type continuationTarget struct {
	SessionKey string
	Channel    string
	ChatID     string
}

const (
	defaultResponse            = "模型返回空响应（模型服务故障或触发 token 上限）。"
	toolLimitResponse          = "达到 `max_tool_iterations` 上限且未产生最终回复；任务需要更多工具步骤时提高 config.json 中的 `max_tool_iterations`。"
	handledToolResponseSummary = "输出已通过工具附件发送。"
	sessionKeyAgentPrefix      = "agent:"
	pendingTurnPrefix          = "pending-"
	providerReloadGracePeriod  = 30 * time.Second
	metadataKeyMessageKind     = "message_kind"
	metadataKeyToolCalls       = "tool_calls"
	metadataKeyOutboundKind    = "outbound_kind"
	messageKindThought         = "thought"
	messageKindToolFeedback    = "tool_feedback"
	messageKindToolCalls       = "tool_calls"
	outboundKindFinal          = "final"
	metadataKeyAccountID       = "account_id"
	metadataKeyGuildID         = "guild_id"
	metadataKeyTeamID          = "team_id"
	metadataKeyReplyToMessage  = "reply_to_message_id"
	metadataKeyParentPeerKind  = "parent_peer_kind"
	metadataKeyParentPeerID    = "parent_peer_id"
)

// registerSharedTools 注册在所有 agent 之间共享的工具（web、message、spawn）。

func (al *AgentLoop) Run(ctx context.Context) error {
	al.running.Store(true)
	defer al.running.Store(false)

	// Hooks 与 MCP 是可选子系统。它们的初始化失败必须让循环降级
	// （记录日志并继续），而不是提前返回：提前返回会让 gateway 保持健康，
	// 但入站 bus 上没有任何消费者，于是每条用户消息都会被静默丢弃
	// （“看似健康却不可用”）。
	if err := al.ensureHooksInitialized(ctx); err != nil {
		logger.ErrorCF("agent", "Hooks initialization failed; continuing without hooks",
			map[string]any{"error": err.Error()})
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		logger.ErrorCF("agent", "MCP initialization failed; continuing without MCP",
			map[string]any{"error": err.Error()})
	}

	// T5.4/T5.7：进程启动时可选地恢复已持久化的活动任务。
	// 默认禁用（autonomy.resume_on_start=false）。
	al.resumeAutonomyTasksOnStart(ctx)

	idleTicker := time.NewTicker(100 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idleTicker.C:
			if !al.running.Load() {
				return nil
			}
		case msg, ok := <-al.bus.InboundChan():
			if !ok {
				return nil
			}

			// 解析此消息的会话键
			sessionKey, agentID, ok := al.resolveSteeringTarget(msg)
			if !ok {
				// 不可路由的消息（例如 system）——立即处理。
				// 注意：system 消息在主 goroutine 中处理，因此会阻塞接收循环，
				// 但能保证会话串行化。
				al.processMessageSync(ctx, msg)
				continue
			}

			// 用唯一的占位哨兵原子地占用会话键，防止出现 TOCTOU 竞争：
			// 同一会话的多条消息可能在任一注册之前都通过 Load 检查。
			// 该占位符确保 GetActiveTurnBySession() 在轮次设置期间绝不会返回 nil。
			// 每个占位符都有唯一的 turnID，以防止跨 worker 清理问题。
			placeholder := &turnState{
				turnID: makePendingTurnID(sessionKey, al.turnSeq.Add(1)),
				phase:  TurnPhaseSetup,
			}
			if _, loaded := al.activeTurnStates.LoadOrStore(sessionKey, placeholder); loaded {
				if al.tryHandleStopCommand(ctx, msg, sessionKey) {
					continue
				}

				msg = al.prepareInboundMessageForAgent(ctx, msg)

				// 此会话已有另一个轮次处于活动（或已预留）状态——入队
				if err := al.enqueueSteeringMessage(sessionKey, agentID, providers.Message{
					Role:    "user",
					Content: msg.Content,
					Media:   append([]string(nil), msg.Media...),
				}); err != nil {
					logger.WarnCF("agent", "Failed to enqueue steering message",
						map[string]any{
							"error":       err.Error(),
							"channel":     msg.Channel,
							"chat_id":     msg.ChatID,
							"session_key": sessionKey,
						})
				}
				continue
			}

			// 会话已占用——启动一个 worker goroutine 获取信号量槽位。
			// 该 goroutine 会立即启动，使主循环能持续排空入站 channel。
			// 该 goroutine 会在信号量上阻塞。
			go func(m bus.InboundMessage, ph *turnState) {
				var releaseSession bool
				// 获取信号量槽位（容量满时阻塞）
				select {
				case al.workerSem <- struct{}{}:
					// 拿到槽位，启动 worker
				case <-ctx.Done():
					// 等待槽位期间上下文被取消——清理
					// 占位符，以防止会话级死锁。
					al.releaseSessionTurnState(sessionKey, nil)
					return
				}

				// 兜底清理：如果占位符从未被真正的 turnState 替换
				// （例如在 runTurn 之前出错），就在此删除它。当 runTurn 正常完成时，
				// clearActiveTurn 会删除真正的 turnState，此处变成空操作
				// （键已不存在）。
				defer func() {
					if releaseSession {
						// 条件删除：仅当该条目仍指向我们的占位符时才移除。
						// 在 panic 到执行此 defer 之间，可能有新消息占用了该槽位。
						if actual, ok := al.activeTurnStates.Load(sessionKey); ok {
							if ts, ok := actual.(*turnState); ok && ts == ph {
								al.releaseSessionTurnState(sessionKey, ts)
							}
						}
						return
					}
					if actual, ok := al.activeTurnStates.Load(sessionKey); ok {
						if ts, ok := actual.(*turnState); ok && strings.HasPrefix(ts.turnID, pendingTurnPrefix) {
							// 占位符仍然存在——runTurn 从未替换它。
							al.releaseSessionTurnState(sessionKey, ts)
						}
					}
				}()

				defer func() {
					if r := recover(); r != nil {
						releaseSession = true
						logger.RecoverPanicNoExit(r)
						logger.ErrorCF("agent", "Worker goroutine panicked",
							map[string]any{
								"session_key": sessionKey,
								"channel":     m.Channel,
								"chat_id":     m.ChatID,
								"panic":       fmt.Sprintf("%v", r),
							})
					}
				}()
				defer func() { <-al.workerSem }() // 释放槽位

				if al.channelManager != nil {
					defer al.channelManager.InvokeTypingStop(m.Channel, m.ChatID)
				}

				if al.takePendingStop(sessionKey) {
					al.releaseSessionTurnState(sessionKey, nil)
					target := &continuationTarget{
						SessionKey: sessionKey,
						Channel:    m.Channel,
						ChatID:     m.ChatID,
					}
					continued, continueErr := al.drainQueuedSteeringContinuations(ctx, target)
					if continueErr != nil {
						al.maybePublishError(ctx, m.Channel, m.ChatID, sessionKey, continueErr)
						return
					}
					if continued != "" {
						al.PublishResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, continued, "", nil)
					}
					return
				}

				al.runTurnWithSteering(ctx, m)
			}(msg, placeholder)

			// TODO: 在入站媒体被 agent 正确消费后重新启用媒体清理。
			// 当前禁用是因为文件会在 LLM 访问其内容之前被删除。
			// defer func() {
			// 	if al.mediaStore != nil && msg.MediaScope != "" {
			// 		if releaseErr := al.mediaStore.ReleaseAll(msg.MediaScope); releaseErr != nil {
			// 			logger.WarnCF("agent", "Failed to release media", map[string]any{
			// 				"scope": msg.MediaScope,
			// 				"error": releaseErr.Error(),
			// 			})
			// 		}
			// 	}
			// }()
		}
	}
}

// processMessageSync 同步处理一条消息（用于不可路由/system 消息）。

// runTurnWithSteering 为一条消息运行完整轮次，并排空其引导队列。

// maybePublishError 发布错误响应，除非错误是 context.Canceled。
// 若应继续处理（非取消错误或无错误）则返回 true，
// 若上下文被取消且调用方应返回则返回 false。

// publishResponseOrError 发布响应；若处理失败则发布错误消息。

func (al *AgentLoop) Stop() {
	al.running.Store(false)
	// 排队的自主轮次不得在关闭/停止请求后存活。
	al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)
}

// Close 释放 agent 会话存储持有的资源。应在 Stop 之后调用。
func (al *AgentLoop) Close() {
	mcpManager := al.mcp.takeManager()

	if mcpManager != nil {
		if err := mcpManager.Close(); err != nil {
			logger.ErrorCF("agent", "Failed to close MCP manager",
				map[string]any{
					"error": err.Error(),
				})
		}
	}

	al.GetRegistry().Close()
	if al.executionReports != nil {
		al.executionReports.close()
	}
	if al.hooks != nil {
		al.hooks.Close()
	}
	al.closeRuntimeEventLogger()
	if al.runtimeEvents != nil && al.ownsRuntimeEvents {
		if err := al.runtimeEvents.Close(); err != nil {
			logger.ErrorCF("agent", "Failed to close runtime event bus",
				map[string]any{
					"error": err.Error(),
				})
		}
	}
}

// MountHook 在 agent 循环上注册一个进程内钩子。

// UnmountHook 移除先前注册的进程内钩子。

type turnEventScope struct {
	agentID    string
	sessionKey string
	turnID     string
	context    *TurnContext
}

// ReloadProviderAndConfig 在正确同步下原子地替换 provider 与 config。
// 它使用 context 允许调用方控制超时。
// 如果重载失败或上下文被取消，则返回错误。
func (al *AgentLoop) ReloadProviderAndConfig(
	ctx context.Context,
	provider providers.LLMProvider,
	cfg *config.Config,
) error {
	// 校验输入
	if provider == nil {
		return fmt.Errorf("provider cannot be nil")
	}
	if cfg == nil {
		return fmt.Errorf("config cannot be nil")
	}

	var registry *AgentRegistry
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.RecoverPanicNoExit(r)
				logger.ErrorCF("agent", "Panic during registry creation",
					map[string]any{"panic": r})
				registry = nil
			}
		}()
		registry = NewAgentRegistry(cfg, provider)
	}()
	if registry == nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context canceled during registry creation: %w", err)
		}
		return fmt.Errorf("registry creation failed")
	}

	// 继续之前再次检查上下文
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context canceled after registry creation: %w", err)
	}

	// 确保共享工具在新的 registry 上重新注册
	registerSharedTools(al, cfg, al.bus, registry, provider)

	// 在写锁下原子地替换 config 与 registry
	// 这确保读者看到一致的一对值
	al.mu.Lock()
	oldRegistry := al.registry

	// 保存新值
	al.cfg = cfg
	al.registry = registry

	// 同时用新配置更新 fallback 链；重建限流器 registry。
	newRL := providers.NewRateLimiterRegistry()
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			newRL.RegisterCandidates(agent.Candidates)
			newRL.RegisterCandidates(agent.LightCandidates)
		}
	}
	al.fallback = providers.NewFallbackChain(providers.NewCooldownTracker(), newRL)

	al.mu.Unlock()

	// 终止上一 agent 代际拥有的后台任务；
	// 新一代以空的任务 registry 启动。
	oldRegistry.CloseBackgroundJobs()

	al.refreshRuntimeEventLogger(cfg)

	oldMCPManager := al.mcp.reset()
	al.hookRuntime.reset(al)
	configureHookManagerFromConfig(al.hooks, cfg)
	if err := al.ensureHooksInitialized(ctx); err != nil {
		logger.WarnCF("agent", "Configured hooks failed to reinitialize after reload",
			map[string]any{"error": err.Error()})
	}
	if oldMCPManager != nil {
		if err := oldMCPManager.Close(); err != nil {
			logger.WarnCF("agent", "Failed to close previous MCP manager during reload",
				map[string]any{"error": err.Error()})
		}
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		logger.WarnCF("agent", "MCP failed to reinitialize after reload",
			map[string]any{"error": err.Error()})
	}

	// 释放锁之后关闭旧 provider
	// 这可避免在关闭时阻塞读者
	if oldProvider, ok := extractProvider(oldRegistry); ok {
		if stateful, ok := oldProvider.(providers.StatefulProvider); ok {
			al.closeReloadedProvider(ctx, stateful)
		}
	}

	logger.InfoCF("agent", "Provider and config reloaded successfully",
		map[string]any{
			"model": cfg.Agents.Defaults.GetModelName(),
		})

	return nil
}

// GetRegistry 返回当前 registry（线程安全）

// GetConfig 返回当前 config（线程安全）

// SetMediaStore 注入用于媒体生命周期管理的 MediaStore。

// SetReloadFunc 设置用于触发配置重载的回调函数。

// inferMediaType 根据文件名和 MIME 内容类型推断媒体类型
// （"image"、"audio"、"video"、"file"）。

// RecordLastChannel 记录此工作区最后活跃的 channel。
// 它使用原子状态保存机制，以防崩溃时数据丢失。

// RecordLastChatID 记录此工作区最后活跃的 chat ID。
// 它使用原子状态保存机制，以防崩溃时数据丢失。

// ProcessHeartbeat 在不使用会话历史的情况下处理心跳请求。
// 每次心跳相互独立，不会累积上下文。

// runAgentLoop 仍是启动轮次并发布轮后工作的顶层外壳。
// runTurn 拥有完整的轮次生命周期。
// 返回：finalContent、modelName、usage、error
func (al *AgentLoop) runAgentLoop(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
) (string, string, *providers.UsageInfo, error) {
	opts = normalizeProcessOptions(opts)
	var err error
	opts, err = resolveTurnProfileOptions(al.GetConfig(), opts)
	if err != nil {
		return "", "", nil, err
	}

	// 记录最后活跃的 channel，用于心跳通知（跳过内部 channel 和 cli）
	if opts.Dispatch.Channel() != "" &&
		opts.Dispatch.ChatID() != "" &&
		!constants.IsInternalChannel(opts.Dispatch.Channel()) {
		channelKey := fmt.Sprintf("%s:%s", opts.Dispatch.Channel(), opts.Dispatch.ChatID())
		if recordErr := al.RecordLastChannel(channelKey); recordErr != nil {
			logger.WarnCF(
				"agent",
				"Failed to record last channel",
				map[string]any{"error": recordErr.Error()},
			)
		}
	}

	ensureSessionMetadata(
		agent.Sessions,
		opts.Dispatch.SessionKey,
		opts.Dispatch.SessionScope,
		opts.Dispatch.SessionAliases,
	)

	turnScope := al.newTurnEventScope(
		agent.ID,
		opts.Dispatch.SessionKey,
		newTurnContext(opts.Dispatch.InboundContext, opts.Dispatch.RouteResult, opts.Dispatch.SessionScope),
	)
	ts := newTurnState(agent, opts, turnScope)
	pipeline := NewPipeline(al)
	result, err := al.runTurn(ctx, ts, pipeline)
	if err != nil {
		// 持久任务仍活跃时重试耗尽，必须留下恢复检查点，
		// 而不是静默完成任务或丢失任务。用户可见的“需要恢复”提示只对自主轮次发送
		// （人类回合已经通过正常路径暴露错误）。
		al.recordTaskProviderErrorCheckpoint(ctx, ts, err)
		return "", "", nil, err
	}
	if result.status == TurnEndStatusAborted {
		return "", "", nil, nil
	}

	for _, followUp := range result.followUps {
		if pubErr := al.bus.PublishInbound(ctx, followUp); pubErr != nil {
			logger.WarnCF("agent", "Failed to publish follow-up after turn",
				map[string]any{
					"turn_id": ts.turnID,
					"error":   pubErr.Error(),
				})
		}
	}

	lastUsage := result.usage

	logger.InfoCF("agent", "[DEBUG] runAgentLoop finalize usage check",
		map[string]any{
			"agent_id":      agent.ID,
			"session_key":   opts.Dispatch.SessionKey,
			"model_name":    result.modelName,
			"has_usage":     lastUsage != nil,
			"send_response": opts.SendResponse,
			"final_len":     len(result.finalContent),
		})

	if opts.SendResponse && result.finalContent != "" {
		agentID, sessionKey, scope := outboundTurnMetadata(
			agent.ID,
			opts.Dispatch.SessionKey,
			opts.Dispatch.SessionScope,
		)
		msg := bus.OutboundMessage{
			Context: outboundContextFromInbound(
				opts.Dispatch.InboundContext,
				opts.Dispatch.Channel(),
				opts.Dispatch.ChatID(),
				opts.Dispatch.ReplyToMessageID(),
			),
			AgentID:      agentID,
			SessionKey:   sessionKey,
			Scope:        scope,
			Content:      result.finalContent,
			ContextUsage: computeContextUsage(agent, opts.Dispatch.SessionKey),
			Usage:        lastUsage,
		}
		if modelName := strings.TrimSpace(result.modelName); modelName != "" {
			if msg.Context.Raw == nil {
				msg.Context.Raw = make(map[string]string, 1)
			}
			msg.Context.Raw["model_name"] = modelName
		}
		markFinalOutbound(&msg)
		al.bus.PublishOutbound(ctx, msg)
	}

	if result.finalContent != "" {
		responsePreview := utils.Truncate(result.finalContent, 120)
		logger.InfoCF("agent", fmt.Sprintf("Response: %s", responsePreview),
			map[string]any{
				"agent_id":     agent.ID,
				"session_key":  opts.Dispatch.SessionKey,
				"iterations":   ts.currentIteration(),
				"final_length": len(result.finalContent),
			})
	}

	// T5.4/T5.5：让活动任务持续运行，直到它完成、阻塞或达到
	// 轮次上限。下一个 P-10 轮次是一个全新的轮次，而不是更深的调用栈帧，
	// 因此 max_tool_iterations/compaction 的语义保持不变。
	al.maybeScheduleAutonomyRound(ctx, agent, ts, result)

	return result.finalContent, result.modelName, lastUsage, nil
}

// selectCandidates 返回用于对话轮次的模型候选和解析后的模型名。
// 当配置了模型路由且入站消息得分低于复杂度阈值时，
// 它返回轻量模型候选而不是主候选。
//
// 返回的 (candidates, model) 对用于一轮内的所有 LLM 调用
// ——工具后续迭代使用与初始调用相同的层级，
// 因此多步工具链不会中途切换模型。

// resolveContextManager 根据配置选择 ContextManager 实现。

// GetStartupInfo 返回已加载工具与技能的信息，用于日志记录。

// formatMessagesForLog 格式化消息，用于日志记录

// formatToolsForLog 格式化工具定义，用于日志记录

// summarizeSession 汇总会话的对话历史。
// findNearestUserMessage 查找距给定索引最近的一条用户消息。
// 它先向后搜索，若未找到用户消息再向前搜索。
// retryLLMCall 带重试逻辑调用 LLM。
// summarizeBatch 汇总一批消息。
// estimateTokens 估算消息列表中的 token 数量。
// 会统计 Content、ToolCalls 参数和 ToolCallID 元数据，
// 因此工具密集的对话不会被系统性低估。

// askSideQuestion 通过创建一个与主对话 provider 不共享状态的
// 隔离 provider 实例来处理 /btw 命令。

// shallowCloneLLMOptions 创建 LLM options map 的浅拷贝。
// 注意：这是浅拷贝——嵌套的 map/slice 是共享的。

// hasMediaRefs 检查是否有任何消息带有媒体引用。

// isolatedSideQuestionProvider 为 /btw 命令创建一个单独的 provider 实例，
// 以避免与主对话 provider 共享状态。

// sideQuestionModelConfig 解析用于附带问题的模型配置。

// sideQuestionModelName 确定用于附带问题的模型名。

// modelNameFromIdentityKey 从标识键中提取模型名。

// closeProviderIfStateful 若 provider 实现了 StatefulProvider 则关闭它。

// makePendingTurnID 为占位轮次生成唯一的轮次 ID。
// 格式："pending-{sessionKey}-{sequence}"

// isNativeSearchProvider 报告给定 LLM provider 是否实现了
// NativeSearchCapable，并为 SupportsNativeSearch 返回 true。

// filterClientWebSearch 返回 tools 的副本，其中已移除客户端
// web_search 工具。当优先使用 provider 原生搜索时使用。

// 从 registry 中提取 provider 的辅助函数，用于清理
