package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/messageutil"
	"github.com/rlds/chagent/pkg/tools"
)

// ====================== 配置与常量 ======================
const (
	// SubTurn 配置的默认值（配置未设置或为零时使用）
	defaultMaxSubTurnDepth       = 3
	defaultMaxConcurrentSubTurns = 5
	defaultConcurrencyTimeout    = 30 * time.Second
	defaultSubTurnTimeout        = 5 * time.Minute
	// maxEphemeralHistorySize 限制临时会话中保存的消息条数。
	// 这能防止长时间运行的子回合不断累积内存。
	maxEphemeralHistorySize = 50
)

var (
	ErrDepthLimitExceeded   = errors.New("sub-turn depth limit exceeded")
	ErrInvalidSubTurnConfig = errors.New("invalid sub-turn config")
	ErrConcurrencyTimeout   = errors.New("timeout waiting for concurrency slot")
)

// getSubTurnConfig 返回应用默认值后的有效 SubTurn 配置。
func (al *AgentLoop) getSubTurnConfig() subTurnRuntimeConfig {
	cfg := al.cfg.Agents.Defaults.SubTurn

	maxDepth := cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxSubTurnDepth
	}

	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrentSubTurns
	}

	concurrencyTimeout := time.Duration(cfg.ConcurrencyTimeoutSec) * time.Second
	if concurrencyTimeout <= 0 {
		concurrencyTimeout = defaultConcurrencyTimeout
	}

	defaultTimeout := time.Duration(cfg.DefaultTimeoutMinutes) * time.Minute
	if defaultTimeout <= 0 {
		defaultTimeout = defaultSubTurnTimeout
	}

	return subTurnRuntimeConfig{
		maxDepth:           maxDepth,
		maxConcurrent:      maxConcurrent,
		concurrencyTimeout: concurrencyTimeout,
		defaultTimeout:     defaultTimeout,
		defaultTokenBudget: cfg.DefaultTokenBudget,
	}
}

// subTurnRuntimeConfig 保存 SubTurn 执行的有效运行时配置。
type subTurnRuntimeConfig struct {
	maxDepth           int
	maxConcurrent      int
	concurrencyTimeout time.Duration
	defaultTimeout     time.Duration
	defaultTokenBudget int
}

// ====================== SubTurn 配置 ======================

// SubTurnConfig 配置子回合（child sub-turn）的执行。
//
// 用法示例：
//
// 同步子回合（Async=false）：
//
//	cfg := SubTurnConfig{
//	    Model: "gpt-4o-mini",
//	    SystemPrompt: "Analyze this code",
//	    Async: false,  // 结果立即返回
//	}
//	result, err := SpawnSubTurn(ctx, cfg)
//	// 在此处直接使用 result
//	processResult(result)
//
// 异步子回合（Async=true）：
//
//	cfg := SubTurnConfig{
//	    Model: "gpt-4o-mini",
//	    SystemPrompt: "Background analysis",
//	    Async: true,  // 结果投递到 channel
//	}
//	result, err := SpawnSubTurn(ctx, cfg)
//	// 结果同时可在父回合的 pendingResults 通道中获取
//	// 父回合会在后续迭代中轮询并处理它
type SubTurnConfig struct {
	Model        string
	Tools        []tools.Tool
	SystemPrompt string
	MaxTokens    int

	// Async 控制结果的投递机制：
	//
	// 当 Async = false（同步子回合）：
	//   - 调用方阻塞直到子回合完成
	//   - 结果仅通过函数返回值返回
	//   - 结果不会投递到父回合的 pendingResults 通道
	//   - 这避免重复投递：调用方立即拿到结果，不需要再经由通道
	//   - 适用场景：调用方需要立即拿到结果才能继续执行
	//   - 示例：需要先处理子回合结果再返回的工具
	//
	// 当 Async = true（异步子回合）：
	//   - 子回合在后台运行（仍会阻塞调用方，但语义上是异步的）
	//   - 结果会投递到父回合的 pendingResults 通道
	//   - 结果同时通过函数返回值返回（为了保持一致）
	//   - 父回合可在后续迭代中轮询 pendingResults 来处理结果
	//   - 适用场景：即发即弃的操作，或结果需要批量处理的场景
	//   - 示例：并行派发多个子回合，稍后再收集结果
	//
	// 重要：Async 标志并不会让调用变成非阻塞。它只控制结果是否经由通道投递。
	// 要实现真正的非阻塞执行，调用方必须把子回合放到单独的 goroutine 中派发。
	Async bool

	// Critical 表示该 SubTurn 的结果很重要，即使父回合优雅结束也应继续运行。
	//
	// 当父回合优雅结束（Finish(false)）时：
	//   - Critical=true：SubTurn 继续运行，结果作为孤儿（orphan）投递
	//   - Critical=false：SubTurn 优雅退出，不报错
	//
	// 当父回合硬中止（Finish(true)）时：
	//   - 无论 Critical 标志如何，所有 SubTurn 都会被取消
	Critical bool

	// Timeout 是该 SubTurn 的最长持续时间。
	// 若 SubTurn 运行时间超过该值，将被取消。
	// 未指定时默认为 5 分钟（defaultSubTurnTimeout）。
	Timeout time.Duration

	// MaxContextRunes 限制传给 SubTurn 的上下文大小（以 rune 计）。
	// 它在 LLM 调用前截断消息历史，以避免上下文窗口溢出。
	//
	// 取值：
	//   0  = 根据模型的 ContextWindow * 0.75 自动计算（默认，推荐）
	//   -1 = 不限制（禁用软截断，只依赖硬性的上下文错误）
	//   >0 = 使用指定的 rune 上限
	//
	// 软限制是触及 provider 硬性上下文窗口上限之前的第一道防线。
	// 一旦超出，会智能地截断较旧的消息，同时保留 system 消息和近期上下文。
	MaxContextRunes int

	// ActualSystemPrompt 作为子 agent 真正的 'system' 角色消息注入。
	// 遗留的 SystemPrompt 字段实际上被用作第一条 'user' 消息（任务描述）。
	ActualSystemPrompt string

	// InitialMessages 在 agent 循环启动前预加载临时会话历史。
	// 供 evaluator-optimizer 模式在多次迭代之间传递完整的工作者上下文。
	InitialMessages []providers.Message

	// InitialTokenBudget 是用于跟踪剩余 token 的共享原子计数器。
	// 若设置，SubTurn 会继承该预算，并在每次 LLM 调用后扣减 token。
	// 若为 nil，SubTurn 会继承父回合的 tokenBudget（如果有）。
	// 供 team 工具在所有团队成员之间强制 token 限制。
	InitialTokenBudget *atomic.Int64

	// TargetAgentID 设置后，子回合将以指定 agent 的身份运行。
	// 此时使用目标 agent 的工作区、模型、工具和 system prompt，
	// 而不是调用方的。若为空，子回合以父 agent 的身份运行。
	TargetAgentID string
}

// ====================== 上下文键 ======================
type agentLoopKeyType struct{}

var agentLoopKey = agentLoopKeyType{}

// WithAgentLoop 将 AgentLoop 注入上下文，供工具访问
func WithAgentLoop(ctx context.Context, al *AgentLoop) context.Context {
	return context.WithValue(ctx, agentLoopKey, al)
}

// AgentLoopFromContext 从上下文中取出 AgentLoop
func AgentLoopFromContext(ctx context.Context) *AgentLoop {
	al, _ := ctx.Value(agentLoopKey).(*AgentLoop)
	return al
}

// ====================== 辅助函数 ======================

func (al *AgentLoop) generateSubTurnID() string {
	return fmt.Sprintf("subturn-%d", al.subTurnCounter.Add(1))
}

// ====================== 核心函数：spawnSubTurn ======================

// AgentLoopSpawner 实现 tools.SubTurnSpawner 接口。
// 这让工具无需循环依赖即可派发子回合。
type AgentLoopSpawner struct {
	al *AgentLoop
}

// SpawnSubTurn 实现 tools.SubTurnSpawner 接口。
func (s *AgentLoopSpawner) SpawnSubTurn(
	ctx context.Context,
	cfg tools.SubTurnConfig,
) (*tools.ToolResult, error) {
	parentTS := turnStateFromContext(ctx)
	if parentTS == nil {
		return nil, errors.New(
			"parent turnState not found in context - cannot spawn sub-turn outside of a turn",
		)
	}

	// 将 tools.SubTurnConfig 转换为 agent.SubTurnConfig
	agentCfg := SubTurnConfig{
		Model:              cfg.Model,
		Tools:              cfg.Tools,
		SystemPrompt:       cfg.SystemPrompt,
		ActualSystemPrompt: cfg.ActualSystemPrompt,
		InitialMessages:    cfg.InitialMessages,
		InitialTokenBudget: cfg.InitialTokenBudget,
		MaxTokens:          cfg.MaxTokens,
		Async:              cfg.Async,
		Critical:           cfg.Critical,
		Timeout:            cfg.Timeout,
		MaxContextRunes:    cfg.MaxContextRunes,
		TargetAgentID:      cfg.TargetAgentID,
	}

	return spawnSubTurn(ctx, s.al, parentTS, agentCfg)
}

// NewSubTurnSpawner 为给定的 AgentLoop 创建 SubTurnSpawner。
func NewSubTurnSpawner(al *AgentLoop) *AgentLoopSpawner {
	return &AgentLoopSpawner{al: al}
}

// SpawnSubTurn 是供工具派发子回合的导出入口。
// 它从上下文中取出 AgentLoop 和父 turnState，然后委托给 spawnSubTurn。
func SpawnSubTurn(ctx context.Context, cfg SubTurnConfig) (*tools.ToolResult, error) {
	al := AgentLoopFromContext(ctx)
	if al == nil {
		return nil, errors.New(
			"AgentLoop not found in context - ensure context is properly initialized",
		)
	}

	parentTS := turnStateFromContext(ctx)
	if parentTS == nil {
		return nil, errors.New(
			"parent turnState not found in context - cannot spawn sub-turn outside of a turn",
		)
	}

	return spawnSubTurn(ctx, al, parentTS, cfg)
}

func spawnSubTurn(
	ctx context.Context,
	al *AgentLoop,
	parentTS *turnState,
	cfg SubTurnConfig,
) (result *tools.ToolResult, err error) {
	// 获取有效的 SubTurn 配置
	rtCfg := al.getSubTurnConfig()

	// 0. 首先获取并发信号量，确保即使早期校验失败它也会被释放。
	// 若父回合已有 maxConcurrentSubTurns 个在运行则会阻塞，并带有超时以避免无限阻塞。
	// 同时尊重上下文取消，这样父回合被中止时不会永远阻塞。
	// 注意：信号量在 runTurn 完成后立即释放（而不是放在 defer 中），
	// 以确保它在清理阶段（异步结果投递）之前被释放；清理阶段可能因 pendingResults
	// 通道已满而阻塞。若把信号量持有到清理结束，父 goroutine 可能在等待信号量槽位时
	// 阻塞，而子回合又因投递结果而阻塞 —— 形成死锁。
	var semAcquired bool
	if parentTS.concurrencySem != nil {
		// 为获取信号量创建带超时的上下文
		timeoutCtx, cancel := context.WithTimeout(ctx, rtCfg.concurrencyTimeout)
		defer cancel()

		select {
		case parentTS.concurrencySem <- struct{}{}:
			semAcquired = true
			defer func() {
				if semAcquired {
					<-parentTS.concurrencySem
				}
			}()
		case <-timeoutCtx.Done():
			// 先检查父上下文 —— 若它已被取消，则传播该错误
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 否则就是我们自己的超时
			return nil, fmt.Errorf("%w: all %d slots occupied for %v",
				ErrConcurrencyTimeout, rtCfg.maxConcurrent, rtCfg.concurrencyTimeout)
		}
	}

	// 1. 深度限制检查
	if parentTS.depth >= rtCfg.maxDepth {
		logger.WarnCF("subturn", "Depth limit exceeded", map[string]any{
			"parent_id": parentTS.turnID,
			"depth":     parentTS.depth,
			"max_depth": rtCfg.maxDepth,
		})
		return nil, ErrDepthLimitExceeded
	}

	// 2. 配置校验：除非设置了 TargetAgentID（此时由目标 agent 提供自己的模型），
	//    否则 Model 为必填项。
	if cfg.Model == "" && cfg.TargetAgentID == "" {
		return nil, ErrInvalidSubTurnConfig
	}

	// 3. 确定子 SubTurn 的超时时间
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = rtCfg.defaultTimeout
	}

	// 4. 创建独立的子上下文（不派生自父 ctx）。
	// 这让子回合在父回合优雅结束后仍能继续运行。
	// 子上下文有自己的超时，用于自我保护。
	childCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	childID := al.generateSubTurnID()

	// 为子回合解析 agent 实例。
	// 设置了 TargetAgentID 时，从注册表查找该 agent，
	// 使子回合使用目标 agent 的工作区、模型、工具和 system prompt。
	// 否则回退到父回合的 agent（既有行为）。
	var baseAgent *AgentInstance
	if cfg.TargetAgentID != "" {
		var ok bool
		baseAgent, ok = al.registry.GetAgent(cfg.TargetAgentID)
		if !ok {
			return nil, fmt.Errorf("target agent %q not found in registry", cfg.TargetAgentID)
		}
	} else {
		baseAgent = parentTS.agent
		if baseAgent == nil {
			baseAgent = al.registry.GetDefaultAgent()
		}
	}
	if baseAgent == nil {
		return nil, errors.New("parent turnState has no agent instance")
	}
	ephemeralStore := newEphemeralSession(nil)
	agent := *baseAgent // 浅拷贝
	agent.Sessions = ephemeralStore
	// 克隆工具注册表，避免子回合注册的工具
	// 污染父回合的注册表。
	if baseAgent.Tools != nil {
		agent.Tools = baseAgent.Tools.Clone()
	}

	// 为子回合创建 processOptions
	dispatch := DispatchRequest{
		SessionKey:     childID,
		UserMessage:    cfg.SystemPrompt,
		Media:          nil,
		InboundContext: cloneInboundContext(parentTS.opts.Dispatch.InboundContext),
	}
	opts := processOptions{
		Dispatch:                dispatch,
		SenderID:                parentTS.opts.Dispatch.SenderID(),
		SenderDisplayName:       parentTS.opts.SenderDisplayName,
		TurnProfile:             parentTS.profile,
		SystemPromptOverride:    cfg.ActualSystemPrompt,
		InitialSteeringMessages: cfg.InitialMessages,
		DefaultResponse:         "",
		EnableSummary:           false,
		SendResponse:            false,
		NoHistory:               true, // SubTurn 不使用会话历史
		SkipInitialSteeringPoll: true,
	}
	if !opts.TurnProfile.Enabled {
		opts.TurnProfile = parentTS.opts.TurnProfile
	}

	// 为子回合创建事件作用域
	scope := al.newTurnEventScope(
		agent.ID,
		childID,
		newTurnContext(opts.Dispatch.InboundContext, opts.Dispatch.RouteResult, opts.Dispatch.SessionScope),
	)

	// 使用新 API 创建子 turnState
	childTS := newTurnState(&agent, opts, scope)

	// 设置 SubTurn 专属字段
	childTS.cancelFunc = cancel
	childTS.critical = cfg.Critical
	childTS.depth = parentTS.depth + 1
	childTS.parentTurnID = parentTS.turnID
	childTS.parentTurnState = parentTS
	childTS.pendingResults = make(chan *tools.ToolResult, 16)
	childTS.concurrencySem = make(chan struct{}, rtCfg.maxConcurrent)
	childTS.al = al                  // 用于硬中止级联的反向引用
	childTS.session = ephemeralStore // 与 agent.Sessions 相同的存储

	// Token 预算的初始化/继承
	// 若显式提供了 InitialTokenBudget（例如由 team 工具提供），则使用它。
	// 否则从父回合的 tokenBudget 继承（用于嵌套的 SubTurn）。
	if cfg.InitialTokenBudget != nil {
		childTS.tokenBudget = cfg.InitialTokenBudget
	} else if parentTS.tokenBudget != nil {
		childTS.tokenBudget = parentTS.tokenBudget
	} else if rtCfg.defaultTokenBudget > 0 {
		// 未设置预算时，应用配置中的默认 token 预算
		budget := &atomic.Int64{}
		budget.Store(int64(rtCfg.defaultTokenBudget))
		childTS.tokenBudget = budget
	}

	// 重要：把 childTS 放入 childCtx，这样 runTurn 内部的代码才能取到它
	childCtx = withTurnState(childCtx, childTS)
	childCtx = WithAgentLoop(childCtx, al) // 将 AgentLoop 传播给子回合

	childTS.ctx = childCtx

	// 注册子回合状态，使 GetAllActiveTurns/Subagents 能找到它
	al.activeTurnStates.Store(childID, childTS)
	defer al.activeTurnStates.Delete(childID)

	// 5. 建立父子关系（线程安全）
	parentTS.mu.Lock()
	parentTS.childTurnIDs = append(parentTS.childTurnIDs, childID)
	parentTS.mu.Unlock()

	// 6. 发出 Spawn 事件
	al.emitEvent(runtimeevents.KindAgentSubTurnSpawn,
		childTS.eventMeta("spawnSubTurn", "subturn.spawn"),
		SubTurnSpawnPayload{
			AgentID:      childTS.agentID,
			Label:        childID,
			ParentTurnID: parentTS.turnID,
		},
	)

	// 7. 延迟清理：投递结果（异步时）、发出 End 事件，并从 panic 中恢复
	defer func() {
		if r := recover(); r != nil {
			logger.RecoverPanicNoExit(r)
			err = fmt.Errorf("subturn panicked: %v", r)
			result = nil
			logger.ErrorCF("subturn", "SubTurn panicked", map[string]any{
				"child_id":  childID,
				"parent_id": parentTS.turnID,
				"panic":     r,
			})
		}

		// 结果投递策略（异步 vs 同步）
		if cfg.Async {
			deliverSubTurnResult(al, parentTS, childID, result)
		}

		status := "completed"
		if err != nil {
			status = "error"
		}
		al.emitEvent(runtimeevents.KindAgentSubTurnEnd,
			childTS.eventMeta("spawnSubTurn", "subturn.end"),
			SubTurnEndPayload{
				AgentID: childTS.agentID,
				Status:  status,
			},
		)
	}()

	// 8. 通过真实的 agent 循环执行子回合。
	pipeline := NewPipeline(al)
	turnRes, turnErr := al.runTurn(childCtx, childTS, pipeline)

	// 在 runTurn 完成后立即释放并发信号量，
	// 早于清理 defer 执行。这可防止如下死锁：
	// - 所有信号量槽位都被处于清理阶段的子回合占用
	// - 清理阶段阻塞在已满的 pendingResults 通道上
	// - 父 goroutine 阻塞在等待信号量槽位
	// - 父回合因阻塞在信号量上而无法消费 pendingResults
	if semAcquired {
		<-parentTS.concurrencySem
		semAcquired = false // 防止 defer 重复释放
	}

	// 将 turnResult 转换为 tools.ToolResult
	if turnErr != nil {
		err = turnErr
		result = &tools.ToolResult{
			Err:    turnErr,
			ForLLM: fmt.Sprintf("SubTurn failed: %v", turnErr),
		}
	} else {
		result = &tools.ToolResult{
			ForLLM:  turnRes.finalContent,
			ForUser: turnRes.finalContent,
		}
	}

	return result, err
}

// ====================== 结果投递 ======================

// deliverSubTurnResult 将子回合结果投递到父回合的 pendingResults 通道。
//
// 重要：本函数只对异步子回合（Async=true）调用。
// 对于同步子回合（Async=false），结果直接通过函数返回值返回，
// 以避免重复投递。
//
// 投递行为：
//   - 若父回合仍在运行：尝试投递到 pendingResults 通道
//   - 若通道已满：发出 agent.subturn.orphan（结果会从通道中丢失，但仍被跟踪）
//   - 若父回合已结束：发出 agent.subturn.orphan（迟到到达）
//
// 线程安全：
//   - 在锁内读取父回合状态，然后在发送到通道之前释放锁
//   - 存在较小的竞态窗口，但可以接受（最坏情况：结果成为孤儿）
//
// 事件发出：
//   - agent.subturn.result_delivered：成功投递到通道
//   - agent.subturn.orphan：投递失败（父回合已结束或通道已满）
func deliverSubTurnResult(al *AgentLoop, parentTS *turnState, childID string, result *tools.ToolResult) {
	// 让 GC 清理 pendingResults 通道；父回合的 Finish 不再关闭它。
	// 我们使用 defer/recover 捕获极不可能出现的通道 panic（假如它曾被关闭）。
	defer func() {
		if r := recover(); r != nil {
			logger.RecoverPanicNoExit(r)
			logger.WarnCF("subturn", "recovered panic sending to pendingResults", map[string]any{
				"parent_id": parentTS.turnID,
				"child_id":  childID,
				"recover":   r,
			})
			if result != nil && al != nil {
				al.emitEvent(runtimeevents.KindAgentSubTurnOrphan,
					parentTS.eventMeta("deliverSubTurnResult", "subturn.orphan"),
					SubTurnOrphanPayload{ParentTurnID: parentTS.turnID, ChildTurnID: childID, Reason: "panic"},
				)
			}
		}
	}()
	parentTS.mu.Lock()
	isFinished := parentTS.isFinished.Load()
	resultChan := parentTS.pendingResults
	parentTS.mu.Unlock()

	// 若父回合已经结束，则将此视为孤儿结果
	if isFinished || resultChan == nil {
		if result != nil && al != nil {
			al.emitEvent(runtimeevents.KindAgentSubTurnOrphan,
				parentTS.eventMeta("deliverSubTurnResult", "subturn.orphan"),
				SubTurnOrphanPayload{ParentTurnID: parentTS.turnID, ChildTurnID: childID, Reason: "parent_finished"},
			)
		}
		return
	}

	// 父回合仍在运行 → 投递结果。
	//
	// 同步：Finish 在 resultsMu.Lock 下关闭 pendingResults；
	// 下面的 send 在 resultsMu.RLock 下执行，因此 close 绝不会与 send 竞争。
	// send 本身是非阻塞的：若缓冲通道已满，我们会短暂等待并重试，同时继续观察 parentTS.Finished()，
	// 因此该 goroutine 绝不会永久阻塞，结果仍可被丢弃（孤儿）。
	for {
		parentTS.resultsMu.RLock()
		select {
		case resultChan <- result:
			parentTS.resultsMu.RUnlock()
			// 投递成功
			if al != nil {
				al.emitEvent(runtimeevents.KindAgentSubTurnResultDelivered,
					parentTS.eventMeta("deliverSubTurnResult", "subturn.result_delivered"),
					SubTurnResultDeliveredPayload{ContentLen: len(result.ForLLM)},
				)
			}
			return
		default:
		}
		parentTS.resultsMu.RUnlock()

		select {
		case <-parentTS.Finished():
			// 等待投递期间父回合已结束。
			// 结果无法投递给 LLM，因此成为孤儿。
			logger.WarnCF("subturn", "parent finished before result could be delivered", map[string]any{
				"parent_id": parentTS.turnID,
				"child_id":  childID,
			})
			if result != nil && al != nil {
				al.emitEvent(
					runtimeevents.KindAgentSubTurnOrphan,
					parentTS.eventMeta("deliverSubTurnResult", "subturn.orphan"),
					SubTurnOrphanPayload{
						ParentTurnID: parentTS.turnID,
						ChildTurnID:  childID,
						Reason:       "parent_finished_waiting",
					},
				)
			}
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// ====================== 其他类型 ======================

// ephemeralSessionStore 是 SubTurn 使用的内存 session.SessionStore。
// 它不持久化到磁盘，并会自动把历史截断到 maxEphemeralHistorySize。
type ephemeralSessionStore struct {
	mu      sync.Mutex
	history []providers.Message
	summary string
}

func newEphemeralSession(initial []providers.Message) ephemeralSessionStoreIface {
	s := &ephemeralSessionStore{}
	if len(initial) > 0 {
		s.history = append(s.history, initial...)
	}
	return s
}

// ephemeralSessionStoreIface 由 *ephemeralSessionStore 实现。
// 声明它是为了让 newEphemeralSession 能返回带类型的接口。
type ephemeralSessionStoreIface interface {
	AddMessage(sessionKey, role, content string)
	AddFullMessage(sessionKey string, msg providers.Message)
	GetHistory(key string) []providers.Message
	GetSummary(key string) string
	SetSummary(key, summary string)
	SetHistory(key string, history []providers.Message)
	TruncateHistory(key string, keepLast int)
	Save(key string) error
	ListSessions() []string
	Close() error
}

func (e *ephemeralSessionStore) AddMessage(_, role, content string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, providers.Message{Role: role, Content: content})
	e.truncateLocked()
}

func (e *ephemeralSessionStore) AddFullMessage(_ string, msg providers.Message) {
	if messageutil.IsTransientAssistantThoughtMessage(msg) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, msg)
	e.truncateLocked()
}

func (e *ephemeralSessionStore) GetHistory(_ string) []providers.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]providers.Message, len(e.history))
	copy(out, e.history)
	return out
}

func (e *ephemeralSessionStore) GetSummary(_ string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.summary
}

func (e *ephemeralSessionStore) SetSummary(_, summary string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.summary = summary
}

func (e *ephemeralSessionStore) SetHistory(_ string, history []providers.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	history = messageutil.FilterInvalidHistoryMessages(history)
	e.history = make([]providers.Message, len(history))
	copy(e.history, history)
	e.truncateLocked()
}

func (e *ephemeralSessionStore) TruncateHistory(_ string, keepLast int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if keepLast <= 0 {
		e.history = nil
		return
	}

	if keepLast >= len(e.history) {
		return
	}
	e.history = e.history[len(e.history)-keepLast:]
}

func (e *ephemeralSessionStore) Save(_ string) error    { return nil }
func (e *ephemeralSessionStore) Close() error           { return nil }
func (e *ephemeralSessionStore) ListSessions() []string { return nil }

func (e *ephemeralSessionStore) truncateLocked() {
	if len(e.history) > maxEphemeralHistorySize {
		e.history = e.history[len(e.history)-maxEphemeralHistorySize:]
	}
}
