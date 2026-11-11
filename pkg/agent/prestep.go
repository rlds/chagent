// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"context"
	"strings"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// pre-step 上下文来源。
//
// DSH 把所有每步上下文注入放进一条有序的 `agent/pre-step` 瀑布，并用中央顺序表
// （`SECTION_ORDERS` / `CONTEXT_ORDERS`）承载顺序，而不是在回合驱动中硬编码。
// chagent 保留同一契约的轻量版本：每个来源声明稳定的顺序和一个纯函数，
// 最多向消息列表追加一条持久上下文消息。
//
// 下面的顺序对应 chess profile 关心的 DSH 序列：
// 易变事实（运行时快照）在任务状态渲染前刷新，工具执行产生的循环卫生提示
// 最后刷新，使模型在下一次请求前立即读到它们。
const (
	preStepOrderWorkspaceContext = 100
	preStepOrderRuntimeContext   = 200
	preStepOrderTaskState        = 300
	preStepOrderContextPressure  = 350
	preStepOrderSkillCatalog     = 400
	preStepOrderLoopGuard        = 500
)

// preStepContextSource 是一个有序的每步上下文注入点。
type preStepContextSource struct {
	ID    string
	Order int
	// Inject 追加零条或多条持久上下文消息并返回新切片。
	// 它不得原地修改输入切片的底层数组，也绝不能让回合失败：失败在注入器内部记录日志。
	Inject func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message
}

// builtinPreStepContextSources 是 chagent 的 pre-step 上下文表。
// 新来源在此按显式顺序追加；必须保持列表有序。
var builtinPreStepContextSources = []preStepContextSource{
	{
		ID:    "context.workspace",
		Order: preStepOrderWorkspaceContext,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.injectWorkspaceContextDelta(ctx, al, messages)
		},
	},
	{
		ID:    "context.runtime",
		Order: preStepOrderRuntimeContext,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.injectRuntimeContext(ctx, al, messages)
		},
	},
	{
		ID:    "context.task_state",
		Order: preStepOrderTaskState,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.injectTaskStateContext(ctx, al, messages)
		},
	},
	{
		ID:    "context.pressure_notice",
		Order: preStepOrderContextPressure,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.injectContextPressureNotice(ctx, al, messages)
		},
	},
	{
		ID:    "context.skill_catalog",
		Order: preStepOrderSkillCatalog,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.injectRegisteredContextSources(ctx, al, messages)
		},
	},
	{
		ID:    "context.loop_guard",
		Order: preStepOrderLoopGuard,
		Inject: func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message {
			return ts.flushStepContextMessages(ctx, al, messages)
		},
	},
}

// runPreStepContextSources 执行有序的 pre-step 上下文表。
// 它是回合循环在 LLM 调用前刷新持久上下文的唯一位置。
// 第 1 次迭代的基线注入仍由 SetupTurn 负责；本函数用于后续迭代
// （以及 SetupTurn 已注入基线后的首次调用，此时各来源是幂等的）。
func (ts *turnState) runPreStepContextSources(
	ctx context.Context,
	al *AgentLoop,
	messages []providers.Message,
) []providers.Message {
	if ts == nil || ts.opts.NoHistory {
		return messages
	}
	iteration := ts.currentIteration()
	for _, source := range builtinPreStepContextSources {
		if source.Inject == nil {
			continue
		}
		before := len(messages)
		messages = source.Inject(ctx, ts, al, messages)
		// 生产可观测性（D2）：pre-step 有序表此前没有任何日志，真实回合只能靠
		// 抓包/JSONL 推断。只在真的注入了消息时打点（正常回合只有少数几处），
		// 使用 Info 级以保证默认日志配置下也能观测。
		if injected := len(messages) - before; injected > 0 {
			logger.InfoCF("agent", "Pre-step context source injected", map[string]any{
				"agent_id":  ts.agentID,
				"turn_id":   ts.turnID,
				"iteration": iteration,
				"source_id": source.ID,
				"order":     source.Order,
				"injected":  injected,
			})
		}
	}
	return messages
}

// enqueueStepContextMessage 为在本回合下一次 LLM 请求前投递而排队一条合成上下文消息。
// 它对应 DSH 的“additionalContexts 被推入下一步收件箱”规则：
// 工具执行绝不直接编辑 assistant/tool 消息，只排队一条消息，由 pre-step 注入器按顺序刷新。
func (ts *turnState) enqueueStepContextMessage(msg providers.Message) {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.pendingStepContexts = append(ts.pendingStepContexts, msg)
}

// pendingStepContextCount 报告有多少排队的步进上下文消息在等待（供测试和诊断使用）。
func (ts *turnState) pendingStepContextCount() int {
	if ts == nil {
		return 0
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.pendingStepContexts)
}

// observeToolCallForLoopGuard 记录一次实际执行的工具尝试，并在命中阈值时排队提示性的重复工具提醒。
// 只能对真正到达执行的调用调用它（被拒绝/跳过的调用不属于该链），且调用方必须串行调用
// （守卫不是并发安全的）。
func (ts *turnState) observeToolCallForLoopGuard(toolName string, args map[string]any) {
	if ts == nil || ts.loopGuard == nil {
		return
	}
	if msg := ts.loopGuard.Observe(toolName, args); msg != nil {
		ts.enqueueStepContextMessage(*msg)
	}
}

// resetLoopGuardChain 清空连续重复链。它在中途注入真实用户指令时调用：
// 跨新用户提示的重复不构成循环（DSH 在每次用户提示时重置链）。
func (ts *turnState) resetLoopGuardChain() {
	if ts == nil || ts.loopGuard == nil {
		return
	}
	ts.loopGuard.Reset()
}

// flushStepContextMessages 把所有排队的步进上下文消息追加到消息列表并持久化，
// 使 replay/resume 和压缩像对待其他持久上下文消息一样对待它们。
func (ts *turnState) flushStepContextMessages(
	ctx context.Context,
	al *AgentLoop,
	messages []providers.Message,
) []providers.Message {
	if ts == nil {
		return messages
	}
	ts.mu.Lock()
	if len(ts.pendingStepContexts) == 0 {
		ts.mu.Unlock()
		return messages
	}
	queued := append([]providers.Message(nil), ts.pendingStepContexts...)
	ts.pendingStepContexts = nil
	ts.mu.Unlock()

	for _, msg := range queued {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		if msg.Role == "" {
			msg.Role = "user"
		}
		if msg.PromptSource == "" {
			msg.PromptSource = string(PromptSourceID("guard:loop_guard"))
		}
		if msg.PromptLayer == "" {
			msg.PromptLayer = string(PromptLayerTurn)
		}
		if msg.PromptSlot == "" {
			msg.PromptSlot = string(PromptSlot("guard"))
		}
		if !ts.opts.NoHistory {
			ts.persistRuntimeContextMessage(ctx, al, msg)
		}
		messages = append(messages, msg)
	}
	return messages
}

// supersedeContextMessages 实现 DSH 对状态型持久上下文的“替换”语义：只应用最新的运行时快照、
// 任务状态和技能目录，因此旧副本会从模型可见历史中丢弃，而不是每刷新一次就累积一份。
// 工作区指令消息是 baseline/delta 记录，故意保留；loop-guard 与上下文压力通知是独立事件，全部保留。
// 这里不改动会话 JSONL 历史；压缩负责缩小它。
func supersedeContextMessages(history []providers.Message) []providers.Message {
	if len(history) == 0 {
		return history
	}
	lastRuntime := -1
	lastTaskState := -1
	lastSkillCatalog := -1
	for i, msg := range history {
		if msg.Context == nil {
			continue
		}
		switch msg.Context.Kind {
		case protocoltypes.ContextKindRuntimeContext:
			lastRuntime = i
		case protocoltypes.ContextKindTaskState:
			lastTaskState = i
		case protocoltypes.ContextKindSkillCatalog:
			lastSkillCatalog = i
		}
	}
	if lastRuntime < 0 && lastTaskState < 0 && lastSkillCatalog < 0 {
		return history
	}
	out := make([]providers.Message, 0, len(history))
	for i, msg := range history {
		if msg.Context != nil {
			switch msg.Context.Kind {
			case protocoltypes.ContextKindRuntimeContext:
				if i != lastRuntime {
					continue
				}
			case protocoltypes.ContextKindTaskState:
				if i != lastTaskState {
					continue
				}
			case protocoltypes.ContextKindSkillCatalog:
				// 目录消息是整表替换（或退役通知）；只有最新一条生效。
				// 因此“已移除”通知必须是最后一条目录消息才能保持可见。
				if i != lastSkillCatalog {
					continue
				}
			}
		}
		out = append(out, msg)
	}
	return out
}

// injectContextPressureNotice 在用量锚定的上下文计量器本回合首次跨过压力区间时，
// 追加提示性的接近上限通知。它只是提示：压缩仍以 isOverContextBudget 作为硬性守卫。
func (ts *turnState) injectContextPressureNotice(
	ctx context.Context,
	al *AgentLoop,
	messages []providers.Message,
) []providers.Message {
	if ts == nil || ts.agent == nil {
		return messages
	}
	var defs []providers.ToolDefinition
	if ts.agent.Tools != nil {
		defs = ts.agent.Tools.ToProviderDefs()
	}
	notice := ts.maybeContextPressureNotice(messages, defs)
	if notice == nil {
		return messages
	}
	ts.persistRuntimeContextMessage(ctx, al, *notice)
	return append(messages, *notice)
}

// injectRegisteredContextSources 运行 ContextBuilder 上注册的每个 ContextSource（T4.1），
// 并追加产生的持久上下文消息。来源排序与去重由 registry 完成；本注入器只持久化 registry 返回的内容。
// 内置技能目录来源就是其中之一：摘要变化时发出替换目录消息；空目录则退役此前发布的名称。
func (ts *turnState) injectRegisteredContextSources(
	ctx context.Context,
	al *AgentLoop,
	messages []providers.Message,
) []providers.Message {
	if ts == nil || ts.agent == nil || ts.agent.ContextBuilder == nil {
		return messages
	}
	var cfg *config.Config
	if al != nil {
		cfg = al.GetConfig()
	}
	req := promptBuildRequestForTurn(ts, messages, "", "", nil, cfg)
	for _, msg := range ts.agent.ContextBuilder.CollectContextMessages(ctx, req) {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		ts.persistRuntimeContextMessage(ctx, al, msg)
		messages = append(messages, msg)
	}
	return messages
}
