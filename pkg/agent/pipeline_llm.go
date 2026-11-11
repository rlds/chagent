// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/constants"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
)

// errLLMCallFailed 标记重试耗尽后的 LLM 调用失败。
// 上层（如 autonomy 的 provider 错误检查点）用 errors.Is 稳定识别该失败，
// 而不是依赖对模型/用户可见错误文案的字符串匹配。
var errLLMCallFailed = errors.New("LLM 调用在重试后失败")

// CallLLM 执行一次带回退支持、hook 调用和重试逻辑的 LLM 调用。
// 它负责 PreLLM 准备、带重试的实际 LLM 调用，以及 AfterLLM 后处理。
// 返回 Control，指示协调器下一步该做什么。
func (p *Pipeline) CallLLM(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) (Control, error) {
	al := p.al
	maxMediaSize := p.Cfg.Agents.Defaults.GetMaxMediaSize()

	// PreLLM：解析媒体引用（第 1 轮除外，此时用户媒体已完成解析）
	if iteration > 1 {
		exec.messages = resolveMediaRefs(exec.messages, p.MediaStore, maxMediaSize, exec.currentTurnStart)
	}

	// PreLLM：优雅终止处理
	exec.gracefulTerminal, _ = ts.gracefulInterruptRequested()
	exec.providerToolDefs = ts.agent.Tools.ToProviderDefs()
	exec.providerToolDefs = filterToolsByTurnProfile(exec.providerToolDefs, ts.profile)

	// 原生 Web 搜索支持
	webSearchEnabled := al.cfg.Tools.IsToolEnabled("web") && turnProfileToolAllowed(ts.profile, "web_search")
	exec.useNativeSearch = webSearchEnabled && al.cfg.Tools.Web.PreferNative &&
		func() bool {
			if ns, ok := ts.agent.Provider.(providers.NativeSearchCapable); ok {
				return ns.SupportsNativeSearch()
			}
			return false
		}()
	if exec.useNativeSearch {
		filtered := make([]providers.ToolDefinition, 0, len(exec.providerToolDefs))
		for _, td := range exec.providerToolDefs {
			if td.Function.Name != "web_search" {
				filtered = append(filtered, td)
			}
		}
		exec.providerToolDefs = filtered
	}

	exec.callMessages = exec.messages
	if exec.gracefulTerminal {
		exec.callMessages = append(append([]providers.Message(nil), exec.messages...), ts.interruptHintMessage())
		exec.providerToolDefs = nil
		ts.markGracefulTerminalUsed()
	}
	if err := p.routeMediaTurn(ts, exec); err != nil {
		return ControlBreak, err
	}

	exec.llmOpts = map[string]any{
		"max_tokens":       ts.agent.MaxTokens,
		"temperature":      ts.agent.Temperature,
		"prompt_cache_key": ts.agent.ID,
	}
	if exec.useNativeSearch {
		exec.llmOpts["native_search"] = true
	}
	applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)

	exec.llmModel = exec.activeModel

	// BeforeLLM hook（LLM 调用前钩子）
	if p.Hooks != nil {
		llmReq, decision := p.Hooks.BeforeLLM(turnCtx, &LLMHookRequest{
			Meta:             ts.eventMeta("runTurn", "turn.llm.request"),
			Context:          cloneTurnContext(ts.turnCtx),
			Model:            exec.llmModel,
			Messages:         exec.callMessages,
			Tools:            exec.providerToolDefs,
			Options:          exec.llmOpts,
			GracefulTerminal: exec.gracefulTerminal,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmReq != nil {
				prevModel := exec.llmModel
				exec.llmModel = llmReq.Model
				exec.callMessages = llmReq.Messages
				exec.providerToolDefs = filterToolsByTurnProfile(llmReq.Tools, ts.profile)
				exec.llmOpts = llmReq.Options
				nativeSearchAllowed := exec.useNativeSearch &&
					turnProfileToolAllowed(ts.profile, "web_search")
				if !nativeSearchAllowed {
					delete(exec.llmOpts, "native_search")
				}
				if strings.TrimSpace(exec.llmModel) != "" && exec.llmModel != prevModel {
					p.applyBeforeLLMModelRewrite(ts, exec)
					applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)
				}
			}
		case HookActionAbortTurn:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			exec.abortedByHook = true
			return ControlBreak, nil
		case HookActionHardAbort:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
	}

	al.emitEvent(
		runtimeevents.KindAgentLLMRequest,
		ts.eventMeta("runTurn", "turn.llm.request"),
		LLMRequestPayload{
			Model:         exec.llmModel,
			MessagesCount: len(exec.callMessages),
			ToolsCount:    len(exec.providerToolDefs),
			MaxTokens:     ts.agent.MaxTokens,
			Temperature:   ts.agent.Temperature,
		},
	)

	logger.DebugCF("agent", "LLM request",
		map[string]any{
			"agent_id":          ts.agent.ID,
			"iteration":         iteration,
			"model":             exec.llmModel,
			"messages_count":    len(exec.callMessages),
			"tools_count":       len(exec.providerToolDefs),
			"max_tokens":        ts.agent.MaxTokens,
			"temperature":       ts.agent.Temperature,
			"system_prompt_len": len(exec.callMessages[0].Content),
		})
	logger.DebugCF("agent", "Full LLM request",
		map[string]any{
			"iteration":     iteration,
			"messages_json": formatMessagesForLog(exec.callMessages),
			"tools_json":    formatToolsForLog(exec.providerToolDefs),
		})

	// 带回退支持的 LLM 调用闭包
	callLLM := func(messagesForCall []providers.Message, toolDefsForCall []providers.ToolDefinition) (*providers.LLMResponse, error) {
		providerCtx, providerCancel := context.WithCancel(turnCtx)
		ts.setProviderCancel(providerCancel)
		defer func() {
			providerCancel()
			ts.clearProviderCancel(providerCancel)
		}()

		al.activeRequestsInc()
		defer al.activeRequestsDec()

		if response, handled, streamErr := p.tryConfiguredStreamingLLM(
			providerCtx,
			ts,
			exec,
			messagesForCall,
			toolDefsForCall,
		); handled {
			return response, streamErr
		}

		runCandidate := func(
			ctx context.Context,
			candidate providers.FallbackCandidate,
		) (*providers.LLMResponse, error) {
			candidateProvider, err := providerForFallbackCandidate(
				ts.agent,
				exec.activeProvider,
				exec.activeCandidates,
				candidate.Provider,
				candidate.Model,
			)
			if err != nil {
				return nil, err
			}
			callOpts := shallowCloneLLMOptions(exec.llmOpts)
			delete(callOpts, "thinking_level")
			candidateCfg := resolveActiveModelConfig(
				p.Cfg,
				ts.agent.Workspace,
				[]providers.FallbackCandidate{candidate},
				candidate.Model,
				p.Cfg.Agents.Defaults.Provider,
			)
			candidateThinking := thinkingSettingsFromModelConfig(candidateCfg)
			applyThinkingOption(callOpts, candidateProvider, candidateThinking, true, ts.agent.ID)
			exec.suppressReasoning = shouldSuppressReasoningFor(candidateThinking)
			return candidateProvider.Chat(ctx, messagesForCall, toolDefsForCall, candidate.Model, callOpts)
		}

		logger.DebugCF("agent", "LLM candidate set", map[string]any{
			"agent_id": ts.agent.ID, "candidates": len(exec.activeCandidates),
			"fallback_chain": p.Fallback != nil,
		})
		if len(exec.activeCandidates) > 1 && p.Fallback != nil {
			var (
				fbResult *providers.FallbackResult
				fbErr    error
			)
			if hasMediaRefs(messagesForCall) {
				fbResult, fbErr = p.Fallback.ExecuteImage(
					providerCtx,
					exec.activeCandidates,
					func(ctx context.Context, provider, model string) (*providers.LLMResponse, error) {
						candidate := providers.FallbackCandidate{Provider: provider, Model: model}
						for _, configured := range exec.activeCandidates {
							if configured.Provider == provider && configured.Model == model {
								candidate = configured
								break
							}
						}
						return runCandidate(ctx, candidate)
					},
				)
			} else {
				fbResult, fbErr = p.Fallback.ExecuteCandidate(
					providerCtx,
					exec.activeCandidates,
					runCandidate,
				)
			}
			if fbErr != nil {
				return nil, fbErr
			}
			if fbResult.Provider != "" && len(fbResult.Attempts) > 0 {
				logger.InfoCF(
					"agent",
					fmt.Sprintf("Fallback: succeeded with %s/%s after %d attempts",
						fbResult.Provider, fbResult.Model, len(fbResult.Attempts)+1),
					map[string]any{"agent_id": ts.agent.ID, "iteration": iteration},
				)
			}
			for _, candidate := range exec.activeCandidates {
				if candidate.StableKey() != fbResult.IdentityKey {
					continue
				}
				exec.llmModelName = resolvedCandidateModelName(
					[]providers.FallbackCandidate{candidate},
					exec.llmModelName,
				)
				break
			}
			return fbResult.Response, nil
		}
		return exec.activeProvider.Chat(providerCtx, messagesForCall, toolDefsForCall, exec.llmModel, exec.llmOpts)
	}

	// 重试循环
	var err error
	maxRetries := p.Cfg.Agents.Defaults.MaxLLMRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}
	backoffSecs := p.Cfg.Agents.Defaults.LLMRetryBackoffSecs
	if backoffSecs <= 0 {
		backoffSecs = 2
	}
	for retry := 0; retry <= maxRetries; retry++ {
		exec.response, err = callLLM(exec.callMessages, exec.providerToolDefs)
		if err == nil {
			break
		}
		if ts.hardAbortRequested() && errors.Is(err, context.Canceled) {
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
		if isConfiguredStreamingVisibleError(err) {
			break
		}

		if hasMediaRefs(exec.callMessages) && isVisionUnsupportedError(err) {
			return ControlBreak, visionUnsupportedModelError(
				exec.llmModelName,
				len(ts.agent.ImageCandidates) > 0,
			)
		}

		errMsg := strings.ToLower(err.Error())
		retryReason, isTransientError := transientLLMRetryReason(err)
		isContextError := !isTransientError && (strings.Contains(errMsg, "context_length_exceeded") ||
			strings.Contains(errMsg, "context window") ||
			strings.Contains(errMsg, "context_window") ||
			strings.Contains(errMsg, "maximum context length") ||
			strings.Contains(errMsg, "token limit") ||
			strings.Contains(errMsg, "too many tokens") ||
			strings.Contains(errMsg, "max_tokens") ||
			strings.Contains(errMsg, "invalidparameter") ||
			strings.Contains(errMsg, "prompt is too long") ||
			strings.Contains(errMsg, "request too large"))

		if isTransientError && retry < maxRetries {
			backoff := time.Duration(retry+1) * time.Duration(backoffSecs) * time.Second
			al.emitEvent(
				runtimeevents.KindAgentLLMRetry,
				ts.eventMeta("runTurn", "turn.llm.retry"),
				LLMRetryPayload{
					Attempt:    retry + 1,
					MaxRetries: maxRetries,
					Reason:     retryReason,
					Error:      err.Error(),
					Backoff:    backoff,
				},
			)
			logger.WarnCF("agent", "Transient LLM error, retrying after backoff", map[string]any{
				"error":   err.Error(),
				"reason":  retryReason,
				"retry":   retry,
				"backoff": backoff.String(),
			})
			if sleepErr := sleepWithContext(turnCtx, backoff); sleepErr != nil {
				if ts.hardAbortRequested() {
					_ = ts.requestHardAbort()
					return ControlBreak, nil
				}
				err = sleepErr
				break
			}
			continue
		}

		if isContextError && retry < maxRetries && !ts.opts.NoHistory {
			al.emitEvent(
				runtimeevents.KindAgentLLMRetry,
				ts.eventMeta("runTurn", "turn.llm.retry"),
				LLMRetryPayload{
					Attempt:    retry + 1,
					MaxRetries: maxRetries,
					Reason:     "context_limit",
					Error:      err.Error(),
				},
			)
			logger.WarnCF(
				"agent",
				"Context window error detected, attempting compression",
				map[string]any{
					"error": err.Error(),
					"retry": retry,
				},
			)

			if retry == 0 && !constants.IsInternalChannel(ts.channel) {
				al.bus.PublishOutbound(ctx, outboundMessageForTurn(
					ts,
					"上下文窗口已超限，正在压缩历史并重试…",
				))
			}

			if compactErr := p.ContextManager.Compact(ctx, &CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonRetry,
				Budget:     ts.agent.ContextWindow,
			}); compactErr != nil {
				logger.WarnCF("agent", "Context overflow compact failed", map[string]any{
					"session_key": ts.sessionKey,
					"error":       compactErr.Error(),
				})
			}
			ts.refreshRestorePointFromSession(ts.agent)
			if asmResp, asmErr := p.ContextManager.Assemble(ctx, &AssembleRequest{
				SessionKey: ts.sessionKey,
				Budget:     ts.agent.ContextWindow,
				MaxTokens:  ts.agent.MaxTokens,
			}); asmErr == nil && asmResp != nil {
				exec.history = asmResp.History
				exec.summary = asmResp.Summary
			}
			contextualSkills := ts.activeSkills
			if ts.agent.ContextBuilder != nil {
				contextualSkills = ts.agent.ContextBuilder.ResolveActiveSkillsForContext(ts.activeSkills)
			}
			ts.recordSkillContextSnapshot(skillContextTriggerContextRetryRebuild, contextualSkills)
			stableHistory, protectedTurnTail := splitHistoryForActiveTurn(
				exec.history,
				ts.persistedMessagesSnapshot(),
			)
			buildMessages := func(trimmedHistory []providers.Message) []providers.Message {
				fullHistory := append(append([]providers.Message(nil), trimmedHistory...), protectedTurnTail...)
				rebuildPromptReq := promptBuildRequestForTurn(ts, fullHistory, exec.summary, "", nil, p.Cfg)
				rebuildPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
				rebuilt := ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
				return resolveMediaRefs(
					rebuilt,
					p.MediaStore,
					maxMediaSize,
					len(rebuilt)-len(protectedTurnTail),
				)
			}
			originalHistoryCount := len(exec.history)
			var fit bool
			var trimmedStableHistory []providers.Message
			trimmedStableHistory, exec.callMessages, fit = trimHistoryToFitContextWindow(
				stableHistory,
				func(trimmedHistory []providers.Message) []providers.Message {
					rebuilt := buildMessages(trimmedHistory)
					if exec.gracefulTerminal {
						return append(append([]providers.Message(nil), rebuilt...), ts.interruptHintMessage())
					}
					return rebuilt
				},
				ts.agent.ContextWindow,
				exec.providerToolDefs,
				ts.agent.MaxTokens,
			)
			exec.history = append(trimmedStableHistory, protectedTurnTail...)
			exec.messages = buildMessages(trimmedStableHistory)
			exec.currentTurnStart = len(exec.messages) - len(protectedTurnTail)
			if exec.gracefulTerminal {
				msgs := append([]providers.Message(nil), exec.messages...)
				exec.callMessages = append(msgs, ts.interruptHintMessage())
			}
			if dropped := originalHistoryCount - len(exec.history); dropped > 0 {
				logger.WarnCF("agent", "Trimmed rebuilt history after context retry compaction", map[string]any{
					"session_key":     ts.sessionKey,
					"retry":           retry,
					"dropped_msgs":    dropped,
					"remaining_msgs":  len(exec.history),
					"context_window":  ts.agent.ContextWindow,
					"max_tokens":      ts.agent.MaxTokens,
					"still_overlimit": !fit,
				})
			} else if !fit {
				logger.WarnCF("agent", "Context still exceeds budget after retry compaction rebuild", map[string]any{
					"session_key":         ts.sessionKey,
					"retry":               retry,
					"history_msgs":        len(exec.history),
					"protected_turn_msgs": len(protectedTurnTail),
					"context_window":      ts.agent.ContextWindow,
					"max_tokens":          ts.agent.MaxTokens,
				})
			}
			if !fit {
				err = fmt.Errorf(
					"重试压缩后上下文窗口仍然超限；拒绝丢弃当前回合的消息：%w",
					err,
				)
				break
			}
			continue
		}
		break
	}

	// Provider 契约防御：callLLM 可能返回 (nil, nil)。不能让 nil response 进入
	// after-LLM 后处理（exec.response.FinishReason/Usage/Model 等会 nil deref，
	// 把回合变成 panic）。视为 provider 错误交给下面的统一错误路径
	// （emit KindAgentError + 返回错误，会话保持结构一致）。
	if err == nil && exec.response == nil {
		err = errors.New("模型服务返回了 nil 响应")
	}

	if err != nil {
		al.emitEvent(
			runtimeevents.KindAgentError,
			ts.eventMeta("runTurn", "turn.error"),
			ErrorPayload{
				Stage:   "llm",
				Message: err.Error(),
			},
		)
		logger.ErrorCF("agent", "LLM call failed",
			map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"model":     exec.llmModel,
				"error":     err.Error(),
			})
		return ControlBreak, fmt.Errorf("%w：%w", errLLMCallFailed, err)
	}

	// AfterLLM hook（LLM 调用后钩子）
	if p.Hooks != nil {
		llmResp, decision := p.Hooks.AfterLLM(turnCtx, &LLMHookResponse{
			Meta:     ts.eventMeta("runTurn", "turn.llm.response"),
			Context:  cloneTurnContext(ts.turnCtx),
			Model:    exec.llmModel,
			Response: exec.response,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmResp != nil && llmResp.Response != nil {
				exec.response = llmResp.Response
			}
		case HookActionAbortTurn:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			exec.abortedByHook = true
			return ControlBreak, nil
		case HookActionHardAbort:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
	}

	// 把 finishReason 保存到 turnState，供 SubTurn 截断检测使用
	if innerTS := turnStateFromContext(ctx); innerTS != nil {
		innerTS.SetLastFinishReason(exec.response.FinishReason)
		if exec.response.Usage != nil {
			innerTS.SetLastUsage(exec.response.Usage)
			// 用量锚定的上下文计量器：把 provider 报告的 prompt 大小与产生该请求的启发式估算配对，
			// 使后续接近上限通知能按真实计费校准。
			innerTS.recordUsageAnchor(exec.response.Usage, exec.callMessages, exec.providerToolDefs)
		}
	}

	if exec.suppressReasoning {
		exec.response.Reasoning = ""
		exec.response.ReasoningContent = ""
		exec.response.ReasoningDetails = nil
	}
	// 优先采用 API 响应返回的模型名，而非配置中的模型名。
	// 这样上报的 model_name 反映实际服务该请求的模型
	// （例如 provider 侧路由、回退或带版本号的模型 ID）。
	if modelFromAPI := strings.TrimSpace(exec.response.Model); modelFromAPI != "" {
		exec.llmModelName = modelFromAPI
	}
	reasoningContent := responseReasoningContent(exec.response)
	shouldPublishPicoToolCallInterim := ts.channel == "pico" && len(exec.response.ToolCalls) > 0
	if shouldPublishPicoToolCallInterim {
		// Pico 的工具调用回合会在下面归一化工具调用负载之后，
		// 把其推理/内容/工具摘要作为结构化序列发布。
	} else if ts.channel == "pico" {
		if exec.streamingPublisher != nil && exec.streamingPublisher.ReasoningPublished() {
			if err := exec.streamingPublisher.FinalizeReasoning(turnCtx, reasoningContent); err != nil {
				logger.WarnCF("agent", "Failed to finalize streamed pico reasoning", map[string]any{
					"channel": ts.channel,
					"chat_id": ts.chatID,
					"error":   err.Error(),
				})
			}
		} else {
			// 在返回时回合上下文被取消之前发布 pico 思考内容。
			// 异步版本可能与回合拆除过程发生竞争，即使 LLM 已产生推理内容，
			// 在 CI 中也会间歇性丢失思考消息。
			al.publishPicoReasoning(turnCtx, reasoningContent, ts.chatID, ts.sessionKey, exec.llmModelName, exec.response.Usage)
		}
	} else {
		go al.handleReasoning(
			turnCtx,
			reasoningContent,
			ts.channel,
			al.targetReasoningChannelID(ts.channel),
		)
	}
	al.emitEvent(
		runtimeevents.KindAgentLLMResponse,
		ts.eventMeta("runTurn", "turn.llm.response"),
		LLMResponsePayload{
			ContentLen:   len(exec.response.Content),
			ToolCalls:    len(exec.response.ToolCalls),
			HasReasoning: exec.response.Reasoning != "" || exec.response.ReasoningContent != "",
		},
	)

	llmResponseFields := map[string]any{
		"agent_id":       ts.agent.ID,
		"iteration":      iteration,
		"content_chars":  len(exec.response.Content),
		"tool_calls":     len(exec.response.ToolCalls),
		"reasoning":      exec.response.Reasoning,
		"target_channel": al.targetReasoningChannelID(ts.channel),
		"channel":        ts.channel,
	}
	if exec.response.Usage != nil {
		llmResponseFields["prompt_tokens"] = exec.response.Usage.PromptTokens
		llmResponseFields["completion_tokens"] = exec.response.Usage.CompletionTokens
		llmResponseFields["total_tokens"] = exec.response.Usage.TotalTokens
	}
	logger.DebugCF("agent", "LLM response", llmResponseFields)

	// 无工具调用路径：先检查 steering 消息，否则直接返回响应
	if len(exec.response.ToolCalls) == 0 || exec.gracefulTerminal {
		responseContent := exec.response.Content
		if responseContent == "" && exec.response.ReasoningContent != "" && ts.channel != "pico" {
			responseContent = exec.response.ReasoningContent
		}
		if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
			cancelConfiguredStreamingLLM(turnCtx, exec)
			logger.InfoCF("agent", "Steering arrived after direct LLM response; continuing turn",
				map[string]any{
					"agent_id":       ts.agent.ID,
					"iteration":      iteration,
					"steering_count": len(steerMsgs),
				})
			exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
			return ControlContinue, nil
		}

		exec.finalContent = responseContent
		logger.InfoCF("agent", "LLM response without tool calls (direct answer)",
			map[string]any{
				"agent_id":      ts.agent.ID,
				"iteration":     iteration,
				"content_chars": len(exec.finalContent),
			})
		return ControlBreak, nil
	}
	cancelConfiguredStreamingLLM(turnCtx, exec)

	// 工具调用路径：归一化并准备执行工具
	exec.normalizedToolCalls = make([]providers.ToolCall, 0, len(exec.response.ToolCalls))
	for _, tc := range exec.response.ToolCalls {
		exec.normalizedToolCalls = append(exec.normalizedToolCalls, providers.NormalizeToolCall(tc))
	}

	toolNames := make([]string, 0, len(exec.normalizedToolCalls))
	for _, tc := range exec.normalizedToolCalls {
		toolNames = append(toolNames, tc.Name)
	}
	logger.InfoCF("agent", "LLM requested tool calls",
		map[string]any{
			"agent_id":  ts.agent.ID,
			"tools":     toolNames,
			"count":     len(exec.normalizedToolCalls),
			"iteration": iteration,
		})

	exec.allResponsesHandled = len(exec.normalizedToolCalls) > 0
	assistantMsg := providers.Message{
		Role:             "assistant",
		Content:          exec.response.Content,
		ModelName:        exec.llmModelName,
		ReasoningContent: reasoningContent,
	}
	for _, tc := range exec.normalizedToolCalls {
		argumentsJSON, _ := json.Marshal(tc.Arguments)
		toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
			exec.response,
			tc,
			exec.messages,
		)
		extraContent := tc.ExtraContent
		if strings.TrimSpace(toolFeedbackExplanation) != "" {
			if extraContent == nil {
				extraContent = &providers.ExtraContent{}
			}
			extraContent.ToolFeedbackExplanation = toolFeedbackExplanation
		}
		thoughtSignature := ""
		if tc.Function != nil {
			thoughtSignature = tc.Function.ThoughtSignature
		}
		assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Name: tc.Name,
			Function: &providers.FunctionCall{
				Name:             tc.Name,
				Arguments:        string(argumentsJSON),
				ThoughtSignature: thoughtSignature,
			},
			ExtraContent:     extraContent,
			ThoughtSignature: thoughtSignature,
		})
	}
	exec.messages = append(exec.messages, assistantMsg)
	if !ts.opts.NoHistory {
		ts.agent.Sessions.AddFullMessage(ts.sessionKey, assistantMsg)
		ts.recordPersistedMessage(assistantMsg)
		ts.ingestMessage(turnCtx, al, assistantMsg)
	}
	if shouldPublishPicoToolCallInterim {
		al.publishPicoToolCallInterim(
			turnCtx,
			ts,
			exec.llmModelName,
			reasoningContent,
			exec.response.Content,
			assistantMsg.ToolCalls,
		)
	}

	return ControlToolLoop, nil
}

func (p *Pipeline) applyBeforeLLMModelRewrite(ts *turnState, exec *turnExecution) {
	if p == nil || ts == nil || ts.agent == nil || exec == nil {
		return
	}
	rawModel := strings.TrimSpace(exec.llmModel)
	if rawModel == "" {
		return
	}

	defaultProvider := "openai"
	if p.Cfg != nil {
		if provider := strings.TrimSpace(p.Cfg.Agents.Defaults.Provider); provider != "" {
			defaultProvider = provider
		}
	}
	defaultProvider = effectiveDefaultProvider(defaultProvider)
	candidates := resolveModelCandidates(p.Cfg, defaultProvider, rawModel, nil)
	exec.activeCandidates = candidates
	exec.activeModel = resolvedCandidateModel(candidates, rawModel)
	exec.llmModel = exec.activeModel
	exec.activeModelConfig = resolveActiveModelConfig(p.Cfg, ts.agent.Workspace, candidates, rawModel, defaultProvider)
}

func providerForFallbackCandidate(
	agent *AgentInstance,
	activeProvider providers.LLMProvider,
	activeCandidates []providers.FallbackCandidate,
	provider string,
	model string,
) (providers.LLMProvider, error) {
	if agent != nil {
		if cp, ok := agent.CandidateProviders[providers.ModelKey(provider, model)]; ok && cp != nil {
			return cp, nil
		}
	}
	if activeProvider == nil {
		return nil, fmt.Errorf("备用模型 %q 没有可用的 provider", model)
	}
	return activeProvider, nil
}

func transientLLMRetryReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	if failErr := providers.ClassifyError(err, "", ""); failErr != nil {
		switch failErr.Reason {
		case providers.FailoverTimeout:
			if failErr.Status >= 500 {
				return "server_error", true
			}
			return "timeout", true
		case providers.FailoverNetwork:
			return "network", true
		case providers.FailoverRateLimit, providers.FailoverOverloaded:
			return "rate_limit", true
		}
	}

	errMsg := strings.ToLower(err.Error())
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(errMsg, "deadline exceeded") ||
		strings.Contains(errMsg, "client.timeout") ||
		strings.Contains(errMsg, "timed out") ||
		strings.Contains(errMsg, "timeout exceeded") {
		return "timeout", true
	}

	if strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "no such host") ||
		strings.Contains(errMsg, "network is unreachable") ||
		strings.Contains(errMsg, "read tcp") ||
		strings.Contains(errMsg, "write tcp") ||
		strings.Contains(errMsg, "eof") {
		return "network", true
	}

	return "", false
}
