// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
)

func (al *AgentLoop) runTurn(ctx context.Context, ts *turnState, pipeline *Pipeline) (turnResult, error) {
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	ts.setTurnCancel(turnCancel)

	// 将 turnState 与 AgentLoop 注入 Context，以便工具（如 spawn）能够取到它们。
	turnCtx = withTurnState(turnCtx, ts)
	turnCtx = WithAgentLoop(turnCtx, al)

	al.registerActiveTurn(ts)
	defer al.clearActiveTurn(ts)

	if al.takePendingStop(ts.sessionKey) {
		_ = ts.requestHardAbort()
	}

	turnStatus := TurnEndStatusCompleted
	defer func() {
		attemptedSkills := ts.attemptedSkillsSnapshot()
		skillContextSnapshots := ts.skillContextSnapshotsSnapshot()
		finalSuccessfulPath := []string(nil)
		if turnStatus == TurnEndStatusCompleted {
			if latest := ts.latestSkillContextSnapshot(); len(latest) > 0 {
				finalSuccessfulPath = latest
			} else {
				finalSuccessfulPath = append([]string(nil), attemptedSkills...)
			}
		}
		al.emitEvent(
			runtimeevents.KindAgentTurnEnd,
			ts.eventMeta("runTurn", "turn.end"),
			TurnEndPayload{
				Status:                turnStatus,
				Workspace:             ts.workspace,
				Iterations:            ts.currentIteration(),
				Duration:              time.Since(ts.startedAt),
				FinalContentLen:       ts.finalContentLen(),
				UserMessage:           ts.userMessage,
				FinalContent:          ts.finalContentSnapshot(),
				ActiveSkills:          append([]string(nil), ts.activeSkills...),
				AttemptedSkills:       attemptedSkills,
				FinalSuccessfulPath:   finalSuccessfulPath,
				SkillContextSnapshots: skillContextSnapshots,
				ToolKinds:             ts.toolKindsSnapshot(),
				ToolExecutions:        ts.toolExecutionsSnapshot(),
			},
		)
	}()

	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	al.emitEvent(
		runtimeevents.KindAgentTurnStart,
		ts.eventMeta("runTurn", "turn.start"),
		TurnStartPayload{
			UserMessage: ts.userMessage,
			MediaCount:  len(ts.media),
		},
	)

	// SetupTurn 抽取仅执行一次的初始化阶段。
	exec, err := pipeline.SetupTurn(turnCtx, ts)
	if err != nil {
		return turnResult{}, err
	}

	// 便于引用的变量，指向回合循环中反复使用的 exec 字段。
	messages := exec.messages
	pendingMessages := exec.pendingMessages
	maxMediaSize := pipeline.Cfg.Agents.Defaults.GetMaxMediaSize()
	finalContent := exec.finalContent

	for ts.currentIteration() < ts.agent.MaxIterations || len(exec.pendingMessages) > 0 || func() bool {
		graceful, _ := ts.gracefulInterruptRequested()
		return graceful
	}() {
		if ts.hardAbortRequested() {
			turnStatus = TurnEndStatusAborted
			return al.abortTurn(ts)
		}

		iteration := ts.currentIteration() + 1
		ts.setIteration(iteration)
		ts.setPhase(TurnPhaseRunning)

		if iteration > 1 {
			// 后续迭代从 exec.pendingMessages 读取，ExecuteTools（或初始轮询）
			// 正是把 steering 消息投放到那里。
			// 此处不再调用 dequeueSteeringMessagesForScope，因为 steering 消息
			// 已由 ExecuteTools 从 al.steering 中取出。
			if len(exec.pendingMessages) > 0 {
				pendingMessages = append(pendingMessages, exec.pendingMessages...)
				exec.pendingMessages = nil
			}
		} else if !ts.opts.SkipInitialSteeringPoll {
			if steerMsgs := al.dequeueSteeringMessagesForScopeWithFallback(ts.sessionKey); len(steerMsgs) > 0 {
				pendingMessages = append(pendingMessages, steerMsgs...)
			}
		}

		// 检查父回合是否已结束（来自 HEAD 的 SubTurn 支持）
		if ts.parentTurnState != nil && ts.IsParentEnded() {
			if !ts.critical {
				logger.InfoCF("agent", "Parent turn ended, non-critical SubTurn exiting gracefully", map[string]any{
					"agent_id":  ts.agentID,
					"iteration": iteration,
					"turn_id":   ts.turnID,
				})
				break
			}
			logger.InfoCF("agent", "Parent turn ended, critical SubTurn continues running", map[string]any{
				"agent_id":  ts.agentID,
				"iteration": iteration,
				"turn_id":   ts.turnID,
			})
		}

		// 轮询待处理的 SubTurn 结果（来自 HEAD）
		if ts.pendingResults != nil {
			select {
			case result, ok := <-ts.pendingResults:
				if ok && result != nil && result.ForLLM != "" {
					content := al.cfg.FilterSensitiveData(result.ForLLM)
					msg := subTurnResultPromptMessage(content)
					pendingMessages = append(pendingMessages, msg)
				}
			default:
				// 暂无可用结果
			}
		}

		// 注入待处理的 steering 消息
		if len(pendingMessages) > 0 {
			resolvedPending := resolveMediaRefs(pendingMessages, al.mediaStore, maxMediaSize, 0)
			totalContentLen := 0
			for i, pm := range pendingMessages {
				messages = append(messages, resolvedPending[i])
				totalContentLen += len(pm.Content)
				if !ts.opts.NoHistory {
					ts.agent.Sessions.AddFullMessage(ts.sessionKey, pm)
					ts.recordPersistedMessage(pm)
					ts.ingestMessage(turnCtx, al, pm)
				}
				logger.InfoCF("agent", "Injected steering message into context",
					map[string]any{
						"agent_id":    ts.agent.ID,
						"iteration":   iteration,
						"content_len": len(pm.Content),
						"media_count": len(pm.Media),
					})
			}
			// 真实用户插话会改变上下文：跨它的重复不构成循环，
			// 因此重置重复工具守卫链（DSH 在每次用户提示时重置）。
			ts.resetLoopGuardChain()
			al.emitEvent(
				runtimeevents.KindAgentSteeringInjected,
				ts.eventMeta("runTurn", "turn.steering.injected"),
				SteeringInjectedPayload{
					Count:           len(pendingMessages),
					TotalContentLen: totalContentLen,
				},
			)
			// 注入后清空 exec.pendingMessages，以免 InitialSteeringMessages
			// 在后续迭代中被重复注入（Issue 2 修复）。
			exec.pendingMessages = nil
		}
		// 在工具/steering 活动之后、下一次模型调用之前，刷新持久上下文
		// （工作区指令增量、运行时快照、任务状态、排队的 loop-guard 通知）。
		// 有序表位于 prestep.go；第 1 次迭代的基线注入由 SetupTurn 负责。
		// NoHistory 回合仍可能收到临时的 loop-guard 通知。
		if iteration > 1 {
			if ts.opts.NoHistory {
				messages = ts.flushStepContextMessages(turnCtx, al, messages)
			} else {
				messages = ts.runPreStepContextSources(turnCtx, al, messages)
				// DSH 的替换语义不只在回合起点成立：回合内每步注入的 runtime /
				// task-state / skill-catalog 也必须在本次请求前收敛为最新一份，
				// 否则模型会同时看到新旧两份状态（会话 JSONL 保留全部副本供审计）。
				messages = supersedeContextMessages(messages)
			}
		}
		// 始终把 messages 同步到 exec.messages，使 CallLLM 能看到最新状态
		exec.messages = messages

		logger.DebugCF("agent", "LLM iteration",
			map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"max":       ts.agent.MaxIterations,
			})

		// 通过 Pipeline 执行 LLM 调用
		ts.setPhase(TurnPhaseRunning)
		// 首参必须是 turnCtx：它携带 turnState（withTurnState）。pipeline_llm 在
		// after-LLM 阶段用 turnStateFromContext(ctx) 记录 LastUsage/LastFinishReason
		// 与用量锚点；传原始 ctx 会让主回合整条路径恒为 nil（只有 SubTurn 生效）。
		ctrl, callErr := pipeline.CallLLM(turnCtx, turnCtx, ts, exec, iteration)
		if callErr != nil {
			turnStatus = TurnEndStatusError
			return turnResult{}, callErr
		}
		messages = exec.messages
		pendingMessages = exec.pendingMessages
		finalContent = exec.finalContent

		switch ctrl {
		case ControlContinue:
			continue
		case ControlBreak:
			// 硬中止：交由 abortTurn 处理（会设置 TurnEndStatusAborted）
			if exec.abortedByHardAbort {
				turnStatus = TurnEndStatusAborted
				return al.abortTurn(ts)
			}
			// Hook 中止（HookActionAbortTurn）：设置 TurnEndStatusError 并返回错误。
			// 与 hard abort 一样先回滚会话：hook 可能在工具执行中途中止回合，
			// 此时带 tool_calls 的 assistant 消息已落库但不会有对应 tool-result，
			// 保留会造成下次请求的悬空 tool_call（provider 可能拒绝）。
			if exec.abortedByHook {
				turnStatus = TurnEndStatusError
				if restoreErr := al.restoreHookAbortedTurn(ts); restoreErr != nil {
					return turnResult{}, restoreErr
				}
				return turnResult{}, fmt.Errorf("hook 请求中止回合")
			}
			// T5.5：持久任务仍活跃时，空响应（无内容、无工具调用）不是最终答案。
			// 注入任务轮次提醒并继续；max_tool_iterations 是上限。
			if finalContent == "" && !exec.allResponsesHandled {
				if state, ok := ts.activeTaskState(); ok {
					ts.appendTaskContinuation(turnCtx, al, exec, state)
					messages = exec.messages
					finalContent = exec.finalContent
					continue
				}
			}
			// 确保空响应回退到 DefaultResponse
			if finalContent == "" {
				finalContent = ts.opts.DefaultResponse
			}
			result, finalizeErr := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
			if finalizeErr != nil {
				turnStatus = TurnEndStatusError
			}
			return result, finalizeErr
		case ControlToolLoop:
			// 通过 Pipeline 执行工具
			toolCtrl := pipeline.ExecuteTools(ctx, turnCtx, ts, exec, iteration)
			switch toolCtrl {
			case ToolControlContinue:
				// 重新读取 exec.messages，因为 ExecuteTools 在返回 ControlContinue 之前
				// 可能已更新它（添加工具结果或跳过消息）
				messages = exec.messages
				continue
			case ToolControlBreak:
				// 硬中止：交由 abortTurn 处理（会设置 TurnEndStatusAborted）
				if exec.abortedByHardAbort {
					turnStatus = TurnEndStatusAborted
					return al.abortTurn(ts)
				}
				// Hook 中止（HookActionAbortTurn）：设置 TurnEndStatusError 并返回错误。
				// 工具执行中途中止时同样先回滚会话，避免 assistant tool_calls 悬空。
				if exec.abortedByHook {
					turnStatus = TurnEndStatusError
					if restoreErr := al.restoreHookAbortedTurn(ts); restoreErr != nil {
						return turnResult{}, restoreErr
					}
					return turnResult{}, fmt.Errorf("hook 请求中止回合")
				}
				// ExecuteTools 返回了 ControlBreak：
				// - allResponsesHandled=true：不应用 DefaultResponse 直接 finalize（exec.finalContent 为空）
				// - allResponsesHandled=false：协调器在 finalize 之前应用 DefaultResponse
				if exec.allResponsesHandled {
					finalContent = ""
				}
				// 持久任务仍处于活跃状态时的空响应不能静默结束任务：
				// 应注入任务轮次提醒并让模型继续（受 MaxIterations 与轮次守卫约束）。
				if finalContent == "" && !exec.allResponsesHandled {
					if state, ok := ts.activeTaskState(); ok {
						ts.appendTaskContinuation(turnCtx, al, exec, state)
						messages = exec.messages
						continue
					}
				}
				result, finalizeErr := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
				if finalizeErr != nil {
					turnStatus = TurnEndStatusError
				}
				return result, finalizeErr
			}
		}
	}

	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	if finalContent == "" {
		if ts.currentIteration() >= ts.agent.MaxIterations && ts.agent.MaxIterations > 0 {
			// T5.5：任务仍活跃时，迭代上限是回合边界，不是答案。
			// 持久化检查点并让驱动调度下一轮；
			// 只有没有活动状态的任务才保留遗留的工具上限响应。
			if !ts.recordIterationLimitCheckpoint(al) {
				finalContent = toolLimitResponse
			}
		} else {
			finalContent = ts.opts.DefaultResponse
		}
	}

	// finalize 前再检查一次硬中止（可能在工具执行期间被设置）
	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	result, err := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
	if err != nil {
		turnStatus = TurnEndStatusError
	}
	return result, err
}

func (al *AgentLoop) abortTurn(ts *turnState) (turnResult, error) {
	ts.setPhase(TurnPhaseAborted)
	if !ts.opts.NoHistory {
		if err := ts.restoreSession(ts.agent); err != nil {
			al.emitEvent(
				runtimeevents.KindAgentError,
				ts.eventMeta("abortTurn", "turn.error"),
				ErrorPayload{
					Stage:   "session_restore",
					Message: err.Error(),
				},
			)
			return turnResult{}, err
		}
	}
	return turnResult{status: TurnEndStatusAborted}, nil
}

// restoreHookAbortedTurn 丢弃被 HookActionAbortTurn 中止回合的半成品持久化状态，
// 与 hard abort 的回滚语义保持一致。
//
// 为什么必须回滚：hook 可以在工具执行中途中止（BeforeTool/AfterTool），此时带
// tool_calls 的 assistant 消息已经写入会话，而对应的 tool-result 永远不会产生。
// 若保留，下一次请求会同时出现 tool_calls 与缺失的 tool 响应，破坏
// “每个 tool_call 必有 tool-result”的结构不变量（多数 provider 会直接拒绝）。
func (al *AgentLoop) restoreHookAbortedTurn(ts *turnState) error {
	if ts == nil || ts.opts.NoHistory {
		return nil
	}
	ts.setPhase(TurnPhaseAborted)
	if err := ts.restoreSession(ts.agent); err != nil {
		al.emitEvent(
			runtimeevents.KindAgentError,
			ts.eventMeta("runTurn", "turn.error"),
			ErrorPayload{
				Stage:   "session_restore",
				Message: err.Error(),
			},
		)
		return err
	}
	return nil
}

func (al *AgentLoop) selectCandidates(
	agent *AgentInstance,
	userMsg string,
	history []providers.Message,
) (candidates []providers.FallbackCandidate, model string, usedLight bool) {
	if agent.Router == nil || len(agent.LightCandidates) == 0 {
		return agent.Candidates, resolvedCandidateModel(agent.Candidates, agent.Model), false
	}

	_, usedLight, score := agent.Router.SelectModel(userMsg, history, agent.Model)
	if !usedLight {
		logger.DebugCF("agent", "Model routing: primary model selected",
			map[string]any{
				"agent_id":  agent.ID,
				"score":     score,
				"threshold": agent.Router.Threshold(),
			})
		return agent.Candidates, resolvedCandidateModel(agent.Candidates, agent.Model), false
	}

	logger.InfoCF("agent", "Model routing: light model selected",
		map[string]any{
			"agent_id":    agent.ID,
			"light_model": agent.Router.LightModel(),
			"score":       score,
			"threshold":   agent.Router.Threshold(),
		})
	return agent.LightCandidates, resolvedCandidateModel(agent.LightCandidates, agent.Router.LightModel()), true
}

func (al *AgentLoop) resolveContextManager() ContextManager {
	name := al.cfg.Agents.Defaults.ContextManager
	if name == "" || name == "legacy" {
		return &legacyContextManager{al: al}
	}
	factory, ok := lookupContextManager(name)
	if !ok {
		logger.WarnCF("agent", "Unknown context manager, falling back to legacy", map[string]any{
			"name": name,
		})
		return &legacyContextManager{al: al}
	}
	cm, err := factory(al.cfg.Agents.Defaults.ContextManagerConfig, al)
	if err != nil {
		logger.WarnCF("agent", "Failed to create context manager, falling back to legacy", map[string]any{
			"name":  name,
			"error": err.Error(),
		})
		return &legacyContextManager{al: al}
	}
	return cm
}

func (al *AgentLoop) askSideQuestion(
	ctx context.Context,
	agent *AgentInstance,
	opts *processOptions,
	question string,
) (string, error) {
	if agent == nil {
		return "", fmt.Errorf("askSideQuestion: no agent available for /btw")
	}

	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("askSideQuestion: %w", fmt.Errorf("Usage: /btw <question>"))
	}

	if opts != nil {
		normalizeProcessOptionsInPlace(opts)
		resolved, err := resolveTurnProfileOptions(al.GetConfig(), *opts)
		if err != nil {
			return "", err
		}
		*opts = resolved
	}

	var media []string
	var channel, chatID, senderID, senderDisplayName string
	if opts != nil {
		media = opts.Media
		channel = opts.Channel
		chatID = opts.ChatID
		senderID = opts.SenderID
		senderDisplayName = opts.SenderDisplayName
	}

	// 构建带上下文的消息，但绝不写入会话历史
	var history []providers.Message
	var summary string
	if opts != nil && !opts.NoHistory {
		if resp, err := al.contextManager.Assemble(ctx, &AssembleRequest{
			SessionKey: opts.SessionKey,
			Budget:     agent.ContextWindow,
			MaxTokens:  agent.MaxTokens,
		}); err == nil && resp != nil {
			history = resp.History
			summary = resp.Summary
		}
	}

	var promptReq PromptBuildRequest
	if opts == nil {
		promptReq = PromptBuildRequest{
			History:           history,
			Summary:           summary,
			CurrentMessage:    question,
			Media:             append([]string(nil), media...),
			Channel:           channel,
			ChatID:            chatID,
			SenderID:          senderID,
			SenderDisplayName: senderDisplayName,
		}
	} else {
		promptReq = promptBuildRequestForProcessOptions(
			agent,
			*opts,
			history,
			summary,
			question,
			media,
		)
	}
	promptReq.SuppressToolUseRule = true
	promptReq.ToolUseFallback = false
	messages := agent.ContextBuilder.BuildMessagesFromPrompt(promptReq)

	maxMediaSize := al.GetConfig().Agents.Defaults.GetMaxMediaSize()
	currentTurnStart := len(messages)
	if strings.TrimSpace(question) != "" || len(media) > 0 {
		currentTurnStart = len(messages) - 1
	}
	messages = resolveMediaRefs(messages, al.mediaStore, maxMediaSize, currentTurnStart)

	activeCandidates, activeModel, usedLight := al.selectCandidates(agent, question, messages)
	selectedModelName := sideQuestionModelName(agent, usedLight)

	llmOpts := map[string]any{
		"max_tokens":       agent.MaxTokens,
		"temperature":      agent.Temperature,
		"prompt_cache_key": agent.ID + ":btw",
	}

	hookModelChanged := false
	sideSuppressReasoning := false
	callProvider := func(
		ctx context.Context,
		candidate providers.FallbackCandidate,
		model string,
		forceModel bool,
		callMessages []providers.Message,
	) (*providers.LLMResponse, error) {
		baseModelName := selectedModelName
		if forceModel && strings.TrimSpace(model) != "" {
			baseModelName = model
		}
		provider, providerModel, modelCfg, cleanup, err := al.isolatedSideQuestionProvider(
			agent,
			baseModelName,
			candidate,
		)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if !forceModel || strings.TrimSpace(model) == "" {
			model = providerModel
		}
		callOpts := llmOpts
		settings := thinkingSettingsFromModelConfig(modelCfg)
		sideSuppressReasoning = shouldSuppressReasoningFor(settings)
		if _, exists := callOpts["thinking_level"]; !exists {
			if settings.configured {
				callOpts = shallowCloneLLMOptions(llmOpts)
				applyThinkingOption(callOpts, provider, settings, false, agent.ID)
			}
		}
		return provider.Chat(ctx, callMessages, nil, model, callOpts)
	}

	turnCtx := newTurnContext(nil, nil, nil)
	if opts != nil {
		turnCtx = newTurnContext(opts.Dispatch.InboundContext, opts.Dispatch.RouteResult, opts.Dispatch.SessionScope)
	}
	llmModel := activeModel
	if al.hooks != nil {
		llmReq, decision := al.hooks.BeforeLLM(ctx, &LLMHookRequest{
			Meta: HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.request",
				turnContext: cloneTurnContext(turnCtx),
			},
			Context:          cloneTurnContext(turnCtx),
			Model:            llmModel,
			Messages:         messages,
			Tools:            nil,
			Options:          llmOpts,
			GracefulTerminal: false,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmReq != nil {
				if strings.TrimSpace(llmReq.Model) != "" && llmReq.Model != llmModel {
					hookModelChanged = true
				}
				llmModel = llmReq.Model
				messages = llmReq.Messages
				llmOpts = llmReq.Options
				delete(llmOpts, "native_search")
			}
		case HookActionAbortTurn:
			reason := decision.Reason
			if reason == "" {
				reason = "hook 请求中止回合"
			}
			return "", fmt.Errorf("hook aborted turn during before_llm: %s", reason)
		case HookActionHardAbort:
			reason := decision.Reason
			if reason == "" {
				reason = "hook 请求中止回合"
			}
			return "", fmt.Errorf("hook aborted turn during before_llm: %s", reason)
		}
	}
	if hookModelChanged {
		// Hook 选定的模型不能再走 hook 之前的回退候选列表，
		// 否则回退执行会调用最初的候选模型，从而静默忽略 hook 的决策。
		activeCandidates = nil
	}

	callSideLLM := func(callMessages []providers.Message) (*providers.LLMResponse, error) {
		if len(activeCandidates) > 1 && al.fallback != nil {
			fbResult, err := al.fallback.ExecuteCandidate(
				ctx,
				activeCandidates,
				func(ctx context.Context, candidate providers.FallbackCandidate) (*providers.LLMResponse, error) {
					return callProvider(ctx, candidate, candidate.Model, false, callMessages)
				},
			)
			if err != nil {
				return nil, err
			}
			return fbResult.Response, nil
		}

		var candidate providers.FallbackCandidate
		if len(activeCandidates) > 0 {
			candidate = activeCandidates[0]
		}
		return callProvider(ctx, candidate, llmModel, hookModelChanged, callMessages)
	}

	// 若模型不支持视觉，则在去掉媒体后重试
	// 注意：视觉重试仅对首次调用生效。如果使用了回退链，
	// 回退 provider 返回的视觉错误不会触发重试。
	var resp *providers.LLMResponse
	var err error
	resp, err = callSideLLM(messages)
	if err != nil && hasMediaRefs(messages) && isVisionUnsupportedError(err) {
		al.emitEvent(
			runtimeevents.KindAgentLLMRetry,
			HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.retry",
				turnContext: cloneTurnContext(turnCtx),
			},
			LLMRetryPayload{
				Attempt:    1,
				MaxRetries: 1,
				Reason:     "vision_unsupported",
				Error:      err.Error(),
				Backoff:    0,
			},
		)
		messagesWithoutMedia := stripMessageMedia(messages)
		resp, err = callSideLLM(messagesWithoutMedia)
	}
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}

	// 应用 after_llm 钩子
	if al.hooks != nil {
		llmResp, decision := al.hooks.AfterLLM(ctx, &LLMHookResponse{
			Meta: HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.response",
				turnContext: cloneTurnContext(turnCtx),
			},
			Context:  cloneTurnContext(turnCtx),
			Model:    llmModel,
			Response: resp,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmResp != nil && llmResp.Response != nil {
				resp = llmResp.Response
			}
		case HookActionAbortTurn, HookActionHardAbort:
			reason := decision.Reason
			if reason == "" {
				reason = "hook 请求中止回合"
			}
			return "", fmt.Errorf("hook aborted turn during after_llm: %s", reason)
		}
	}
	if sideSuppressReasoning {
		resp.Reasoning = ""
		resp.ReasoningContent = ""
		resp.ReasoningDetails = nil
	}

	return sideQuestionResponseContent(resp), nil
}

func (al *AgentLoop) isolatedSideQuestionProvider(
	agent *AgentInstance,
	baseModelName string,
	candidate providers.FallbackCandidate,
) (providers.LLMProvider, string, *config.ModelConfig, func(), error) {
	if agent == nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: no agent available for /btw")
	}

	modelCfg, err := al.sideQuestionModelConfig(agent, baseModelName, candidate)
	if err != nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: %w", err)
	}

	factory := al.providerFactory
	if factory == nil {
		factory = providers.CreateProviderFromConfig
	}
	provider, modelID, err := factory(modelCfg)
	if err != nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: %w", err)
	}

	cleanup := func() {
		closeProviderIfStateful(provider)
	}
	return provider, modelID, modelCfg, cleanup, nil
}

func (al *AgentLoop) sideQuestionModelConfig(
	agent *AgentInstance,
	baseModelName string,
	candidate providers.FallbackCandidate,
) (*config.ModelConfig, error) {
	if agent == nil {
		return nil, fmt.Errorf("sideQuestionModelConfig: no agent available for /btw")
	}

	if name := modelAliasFromCandidateIdentityKey(candidate.IdentityKey); name != "" {
		modelCfg, err := resolvedModelConfig(al.GetConfig(), name, agent.Workspace)
		if err == nil {
			return modelCfg, nil
		}
		// 回退：查找失败时创建一个最小配置
	}

	// 较早的 identity key 使用 provider/model 形式；仍按其中的模型名解析。
	if name := modelNameFromIdentityKey(candidate.IdentityKey); name != "" {
		modelCfg, err := resolvedModelConfig(al.GetConfig(), name, agent.Workspace)
		if err == nil {
			return modelCfg, nil
		}
		// 回退：查找失败时创建一个最小配置
	}

	if candidate.Provider != "" && candidate.Model != "" {
		candidateRef := providers.NormalizeProvider(candidate.Provider) + "/" + candidate.Model
		if modelCfg, err := resolvedModelConfig(al.GetConfig(), candidateRef, agent.Workspace); err == nil {
			return modelCfg, nil
		}
		return &config.ModelConfig{
			ModelName: candidateRef,
			Model:     candidateRef,
			Workspace: agent.Workspace,
		}, nil
	}

	// 否则清理基础模型名并使用它
	baseModelName = strings.TrimSpace(baseModelName)
	modelCfg, err := resolvedModelConfig(al.GetConfig(), baseModelName, agent.Workspace)
	if err != nil {
		// 回退：为测试场景创建一个最小配置
		model := strings.TrimSpace(baseModelName)
		if candidate.Model != "" {
			model = candidate.Model
		}
		if candidate.Provider != "" && candidate.Model != "" {
			model = providers.NormalizeProvider(candidate.Provider) + "/" + candidate.Model
		} else {
			model = ensureProtocolModel(model)
		}
		return &config.ModelConfig{
			ModelName: baseModelName,
			Model:     model,
			Workspace: agent.Workspace,
		}, nil
	}

	// 如果候选指定了不同的 provider/model，则据此覆盖
	clone := *modelCfg
	return &clone, nil
}
