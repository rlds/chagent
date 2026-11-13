// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/constants"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools"
	integrationtools "github.com/rlds/chagent/pkg/tools/integration"
	"github.com/rlds/chagent/pkg/utils"
)

func toolErrorSummary(result *tools.ToolResult) string {
	if result == nil || !result.IsError {
		return ""
	}
	content := strings.TrimSpace(result.ContentForLLM())
	if content == "" && result.Err != nil {
		content = strings.TrimSpace(result.Err.Error())
	}
	return utils.Truncate(content, 200)
}

func inferSkillNamesFromToolCall(ts *turnState, toolName string, toolArgs map[string]any) []string {
	if ts == nil || toolName != "read_file" {
		return nil
	}

	rawPath, ok := toolArgs["path"].(string)
	if !ok {
		return nil
	}
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return nil
	}

	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		cleanPath = filepath.Join(ts.workspace, cleanPath)
	}
	if filepath.Base(cleanPath) != "SKILL.md" {
		return nil
	}

	var roots []string
	if ts.agent != nil && ts.agent.ContextBuilder != nil {
		roots = ts.agent.ContextBuilder.skillRoots()
	}
	if len(roots) == 0 && strings.TrimSpace(ts.workspace) != "" {
		roots = []string{filepath.Join(ts.workspace, "skills")}
	}

	found := make(map[string]struct{})
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(root), cleanPath)
		if err != nil {
			continue
		}
		if rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 || parts[1] != "SKILL.md" {
			continue
		}

		skillName := strings.TrimSpace(parts[0])
		if skillName == "" {
			continue
		}
		if ts.agent != nil && ts.agent.ContextBuilder != nil {
			if canonical, ok := ts.agent.ContextBuilder.ResolveSkillName(skillName); ok {
				skillName = canonical
			}
		}
		found[skillName] = struct{}{}
	}

	if len(found) == 0 {
		return nil
	}

	names := make([]string, 0, len(found))
	for skillName := range found {
		names = append(names, skillName)
	}
	sort.Strings(names)
	return names
}

// ExecuteTools 执行工具循环，处理 BeforeTool/ApproveTool/AfterTool 钩子、
// 带异步回调的工具执行、媒体投递以及 steering 注入。
// 返回的 ToolControl 指示协调器下一步该做什么：
//   - ToolControlContinue：所有工具结果均已处理，且存在 pendingMessages 或 steering，继续回合
//   - ToolControlBreak：工具循环已退出，交由协调器处理 hardAbort/finalContent/finalize
func (p *Pipeline) ExecuteTools(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) ToolControl {
	al := p.al
	normalizedToolCalls := exec.normalizedToolCalls

	ts.setPhase(TurnPhaseTools)
	messages := exec.messages
	handledAttachments := make([]providers.Attachment, 0)

	// DSH 风格并发批次路径（见 tool_batch.go）。它刻意保守：
	// 只有在未挂载任何工具 hook、至少两个调用且存在相邻并行安全对、
	// 且没有排队 steering 时才启用。其他所有情况保持下方原有串行循环不变。
	if p.shouldUseConcurrentToolBatch(ts, exec) {
		return p.executeToolsConcurrentBatch(ctx, turnCtx, ts, exec, iteration)
	}

toolLoop:
	for i, tc := range normalizedToolCalls {
		if ts.hardAbortRequested() {
			exec.abortedByHardAbort = true
			return ToolControlBreak
		}

		toolName := tc.Name
		toolArgs := cloneStringAnyMap(tc.Arguments)
		denyByTurnProfile := func() bool {
			if turnProfileToolAllowed(ts.profile, toolName) {
				return false
			}
			exec.allResponsesHandled = false
			denyContent := fmt.Sprintf("工具 %q 不在当前回合配置允许范围内。", toolName)
			al.emitEvent(
				runtimeevents.KindAgentToolExecSkipped,
				ts.eventMeta("runTurn", "turn.tool.skipped"),
				ToolExecSkippedPayload{
					Tool:   toolName,
					Reason: denyContent,
				},
			)
			deniedMsg := providers.Message{
				Role:       "tool",
				Content:    denyContent,
				ToolCallID: tc.ID,
			}
			messages = append(messages, deniedMsg)
			if !ts.opts.NoHistory {
				ts.agent.Sessions.AddFullMessage(ts.sessionKey, deniedMsg)
				ts.recordPersistedMessage(deniedMsg)
			}
			return true
		}

		if denyByTurnProfile() {
			continue
		}

		if al.hooks != nil {
			toolReq, decision := al.hooks.BeforeTool(turnCtx, &ToolCallHookRequest{
				Meta:      ts.eventMeta("runTurn", "turn.tool.before"),
				Context:   cloneTurnContext(ts.turnCtx),
				Tool:      toolName,
				Arguments: toolArgs,
			})
			switch decision.normalizedAction() {
			case HookActionContinue, HookActionModify:
				if toolReq != nil {
					toolName = toolReq.Tool
					toolArgs = toolReq.Arguments
				}
			case HookActionRespond:
				if toolReq != nil && toolReq.HookResult != nil {
					hookResult := toolReq.HookResult

					// 循环卫生：该调用已到达执行阶段，因此计入重复工具守卫链。
					ts.observeToolCallForLoopGuard(toolName, toolArgs)

					argsJSON, _ := json.Marshal(toolArgs)
					argsPreview := utils.Truncate(string(argsJSON), 200)
					logger.InfoCF("agent", fmt.Sprintf("Tool call (hook respond): %s(%s)", toolName, argsPreview),
						map[string]any{
							"agent_id":  ts.agent.ID,
							"tool":      toolName,
							"iteration": iteration,
						})

					al.emitEvent(
						runtimeevents.KindAgentToolExecStart,
						ts.eventMeta("runTurn", "turn.tool.start"),
						ToolExecStartPayload{
							Tool:      toolName,
							Arguments: cloneEventArguments(toolArgs),
						},
					)

					if shouldPublishToolFeedback(al.cfg, ts) && ts.channel != "pico" {
						toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
							exec.response,
							tc,
							messages,
						)
						feedbackMsg := utils.FormatToolFeedbackMessage(
							toolName,
							toolFeedbackExplanation,
							toolFeedbackArgsPreview(toolArgs, 0),
						)
						fbCtx, fbCancel := context.WithTimeout(turnCtx, 3*time.Second)
						_ = al.bus.PublishOutbound(fbCtx, outboundMessageForTurnWithOptions(
							ts,
							feedbackMsg,
							outboundTurnMessageOptions{kind: messageKindToolFeedback},
						))
						fbCancel()
					}

					toolDuration := time.Duration(0)

					shouldSendForUser := !hookResult.Silent && hookResult.ForUser != "" &&
						(ts.opts.SendResponse || hookResult.ResponseHandled)
					if shouldSendForUser {
						al.bus.PublishOutbound(ctx, bus.OutboundMessage{
							Context: bus.InboundContext{
								Channel: ts.channel,
								ChatID:  ts.chatID,
								Raw: map[string]string{
									"is_tool_call": "true",
								},
							},
							Content: hookResult.ForUser,
						})
					}

					if len(hookResult.Media) > 0 && hookResult.ResponseHandled {
						parts := make([]bus.MediaPart, 0, len(hookResult.Media))
						for _, ref := range hookResult.Media {
							part := bus.MediaPart{Ref: ref}
							if al.mediaStore != nil {
								if _, meta, err := al.mediaStore.ResolveWithMeta(ref); err == nil {
									part.Filename = meta.Filename
									part.ContentType = meta.ContentType
									part.Type = inferMediaType(meta.Filename, meta.ContentType)
								}
							}
							parts = append(parts, part)
						}
						outboundMedia := bus.OutboundMediaMessage{
							Channel: ts.channel,
							ChatID:  ts.chatID,
							Context: outboundContextFromInbound(
								ts.opts.Dispatch.InboundContext,
								ts.channel,
								ts.chatID,
								ts.opts.Dispatch.ReplyToMessageID(),
							),
							AgentID:    ts.agent.ID,
							SessionKey: ts.sessionKey,
							Scope:      outboundScopeFromSessionScope(ts.opts.Dispatch.SessionScope),
							Parts:      parts,
						}
						if al.channelManager != nil && ts.channel != "" && !constants.IsInternalChannel(ts.channel) {
							if err := al.channelManager.SendMedia(ctx, outboundMedia); err != nil {
								logger.WarnCF("agent", "Failed to deliver hook media",
									map[string]any{
										"agent_id": ts.agent.ID,
										"tool":     toolName,
										"channel":  ts.channel,
										"chat_id":  ts.chatID,
										"error":    err.Error(),
									})
								hookResult.IsError = true
								hookResult.ForLLM = fmt.Sprintf("附件交付失败：%v", err)
							} else {
								handledAttachments = append(
									handledAttachments,
									buildProviderAttachments(al.mediaStore, hookResult.Media)...,
								)
							}
						} else if al.bus != nil {
							al.bus.PublishOutboundMedia(ctx, outboundMedia)
							hookResult.ResponseHandled = false
						}
					}

					if !hookResult.ResponseHandled {
						exec.allResponsesHandled = false
					}

					contentForLLM := hookResult.ContentForLLM()
					if al.cfg.Tools.IsFilterSensitiveDataEnabled() {
						contentForLLM = al.cfg.FilterSensitiveData(contentForLLM)
					}

					var toolResultMedia []string
					if len(hookResult.Media) > 0 && !hookResult.ResponseHandled {
						hookResult.ArtifactTags = buildArtifactTags(al.mediaStore, hookResult.Media)
						contentForLLM = hookResult.ContentForLLM()
						if al.cfg.Tools.IsFilterSensitiveDataEnabled() {
							contentForLLM = al.cfg.FilterSensitiveData(contentForLLM)
						}
						toolResultMedia = append(toolResultMedia, hookResult.Media...)
					}
					toolResultMsg := toolResultPromptMessage(contentForLLM, tc.ID, toolResultMedia)

					al.emitEvent(
						runtimeevents.KindAgentToolExecEnd,
						ts.eventMeta("runTurn", "turn.tool.end"),
						ToolExecEndPayload{
							Tool:       toolName,
							Duration:   toolDuration,
							ForLLMLen:  len(contentForLLM),
							ForUserLen: len(hookResult.ForUser),
							IsError:    hookResult.IsError,
							Async:      hookResult.Async,
						},
					)
					ts.recordToolExecution(
						toolName,
						!hookResult.IsError,
						toolErrorSummary(hookResult),
						inferSkillNamesFromToolCall(ts, toolName, toolArgs),
					)
					ts.recordContextTouch(toolName, toolArgs, hookResult.IsError)

					messages = append(messages, toolResultMsg)
					if !ts.opts.NoHistory {
						ts.agent.Sessions.AddFullMessage(ts.sessionKey, toolResultMsg)
						ts.recordPersistedMessage(toolResultMsg)
						ts.ingestMessage(turnCtx, al, toolResultMsg)
					}

					if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
						exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
					}

					skipReason := ""
					skipMessage := ""
					if len(exec.pendingMessages) > 0 {
						skipReason = "queued user steering message"
						skipMessage = "因排队中的用户消息而跳过。"
					} else if gracefulPending, _ := ts.gracefulInterruptRequested(); gracefulPending {
						skipReason = "graceful interrupt requested"
						skipMessage = "因请求了优雅中断而跳过。"
					}

					if skipReason != "" {
						remaining := len(normalizedToolCalls) - i - 1
						if remaining > 0 {
							logger.InfoCF("agent", "Turn checkpoint: skipping remaining tools after hook respond",
								map[string]any{
									"agent_id":  ts.agent.ID,
									"completed": i + 1,
									"skipped":   remaining,
									"reason":    skipReason,
								})
							for j := i + 1; j < len(normalizedToolCalls); j++ {
								skippedTC := normalizedToolCalls[j]
								al.emitEvent(
									runtimeevents.KindAgentToolExecSkipped,
									ts.eventMeta("runTurn", "turn.tool.skipped"),
									ToolExecSkippedPayload{
										Tool:   skippedTC.Name,
										Reason: skipReason,
									},
								)
								skippedMsg := providers.Message{
									Role:       "tool",
									Content:    skipMessage,
									ToolCallID: skippedTC.ID,
								}
								messages = append(messages, skippedMsg)
								if !ts.opts.NoHistory {
									ts.agent.Sessions.AddFullMessage(ts.sessionKey, skippedMsg)
									ts.recordPersistedMessage(skippedMsg)
								}
							}
						}
						break toolLoop
					}

					if ts.pendingResults != nil {
						select {
						case result, ok := <-ts.pendingResults:
							if ok && result != nil && result.ForLLM != "" {
								content := al.cfg.FilterSensitiveData(result.ForLLM)
								msg := subTurnResultPromptMessage(content)
								messages = append(messages, msg)
								if !ts.opts.NoHistory {
									ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
								}
							}
						default:
						}
					}

					continue
				}
				logger.WarnCF("agent", "Hook returned respond action but no HookResult provided",
					map[string]any{
						"agent_id": ts.agent.ID,
						"tool":     toolName,
						"action":   "respond",
					})
			case HookActionDenyTool:
				exec.allResponsesHandled = false
				denyContent := hookDeniedToolContent("工具执行被 hook 拒绝", decision.Reason)
				al.emitEvent(
					runtimeevents.KindAgentToolExecSkipped,
					ts.eventMeta("runTurn", "turn.tool.skipped"),
					ToolExecSkippedPayload{
						Tool:   toolName,
						Reason: denyContent,
					},
				)
				deniedMsg := providers.Message{
					Role:       "tool",
					Content:    denyContent,
					ToolCallID: tc.ID,
				}
				messages = append(messages, deniedMsg)
				if !ts.opts.NoHistory {
					ts.agent.Sessions.AddFullMessage(ts.sessionKey, deniedMsg)
					ts.recordPersistedMessage(deniedMsg)
				}
				continue
			case HookActionAbortTurn:
				exec.abortedByHook = true
				return ToolControlBreak
			case HookActionHardAbort:
				_ = ts.requestHardAbort()
				exec.abortedByHardAbort = true
				return ToolControlBreak
			}
		}

		if al.hooks != nil {
			approval := al.hooks.ApproveTool(turnCtx, &ToolApprovalRequest{
				Meta:      ts.eventMeta("runTurn", "turn.tool.approve"),
				Context:   cloneTurnContext(ts.turnCtx),
				Tool:      toolName,
				Arguments: toolArgs,
			})
			if !approval.Approved {
				exec.allResponsesHandled = false
				denyContent := hookDeniedToolContent("工具执行被审批 hook 拒绝", approval.Reason)
				al.emitEvent(
					runtimeevents.KindAgentToolExecSkipped,
					ts.eventMeta("runTurn", "turn.tool.skipped"),
					ToolExecSkippedPayload{
						Tool:   toolName,
						Reason: denyContent,
					},
				)
				deniedMsg := providers.Message{
					Role:       "tool",
					Content:    denyContent,
					ToolCallID: tc.ID,
				}
				messages = append(messages, deniedMsg)
				if !ts.opts.NoHistory {
					ts.agent.Sessions.AddFullMessage(ts.sessionKey, deniedMsg)
					ts.recordPersistedMessage(deniedMsg)
				}
				continue
			}
		}

		if denyByTurnProfile() {
			continue
		}

		// 循环卫生：在执行前计数尝试，使重复相同调用可在下一步被提示性提醒打断。
		// 被拒绝/跳过的调用刻意不计数。
		ts.observeToolCallForLoopGuard(toolName, toolArgs)

		argsJSON, _ := json.Marshal(toolArgs)
		argsPreview := utils.Truncate(string(argsJSON), 200)
		logger.InfoCF("agent", fmt.Sprintf("Tool call: %s(%s)", toolName, argsPreview),
			map[string]any{
				"agent_id":  ts.agent.ID,
				"tool":      toolName,
				"iteration": iteration,
			})
		al.emitEvent(
			runtimeevents.KindAgentToolExecStart,
			ts.eventMeta("runTurn", "turn.tool.start"),
			ToolExecStartPayload{
				Tool:      toolName,
				Arguments: cloneEventArguments(toolArgs),
			},
		)

		if shouldPublishToolFeedback(al.cfg, ts) && ts.channel != "pico" {
			toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
				exec.response,
				tc,
				messages,
			)
			feedbackMsg := utils.FormatToolFeedbackMessage(
				toolName,
				toolFeedbackExplanation,
				toolFeedbackArgsPreview(toolArgs, 0),
			)
			fbCtx, fbCancel := context.WithTimeout(turnCtx, 3*time.Second)
			_ = al.bus.PublishOutbound(fbCtx, outboundMessageForTurnWithOptions(
				ts,
				feedbackMsg,
				outboundTurnMessageOptions{kind: messageKindToolFeedback},
			))
			fbCancel()
		}

		toolCallID := tc.ID
		asyncToolName := toolName
		asyncCallback := func(_ context.Context, result *tools.ToolResult) {
			if !result.Silent && result.ForUser != "" {
				outCtx, outCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer outCancel()
				_ = al.bus.PublishOutbound(outCtx, outboundMessageForTurn(ts, result.ForUser))
			}

			content := result.ContentForLLM()
			if content == "" {
				return
			}

			content = al.cfg.FilterSensitiveData(content)

			logger.InfoCF("agent", "Async tool completed, publishing result",
				map[string]any{
					"tool":        asyncToolName,
					"content_len": len(content),
					"channel":     ts.channel,
				})
			al.emitEvent(
				runtimeevents.KindAgentFollowUpQueued,
				ts.scope.meta(iteration, "runTurn", "turn.follow_up.queued"),
				FollowUpQueuedPayload{
					SourceTool: asyncToolName,
					ContentLen: len(content),
				},
			)
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer pubCancel()
			_ = al.bus.PublishInbound(pubCtx, bus.InboundMessage{
				Context: bus.InboundContext{
					Channel:  "system",
					ChatID:   fmt.Sprintf("%s:%s", ts.channel, ts.chatID),
					ChatType: "direct",
					SenderID: fmt.Sprintf("async:%s", asyncToolName),
				},
				Content: content,
			})
		}

		toolStart := time.Now()
		execCtx := tools.WithToolInboundContext(
			turnCtx,
			ts.channel,
			ts.chatID,
			ts.opts.Dispatch.MessageID(),
			ts.opts.Dispatch.ReplyToMessageID(),
		)
		execCtx = tools.WithToolSessionContext(
			execCtx,
			ts.agent.ID,
			ts.sessionKey,
			ts.opts.Dispatch.SessionScope,
		)
		execCtx = integrationtools.WithTurnSource(execCtx, turnSourceFromString(ts.opts.TurnSource))
		toolResult := ts.agent.Tools.ExecuteWithContext(
			execCtx,
			toolName,
			toolArgs,
			ts.channel,
			ts.chatID,
			asyncCallback,
		)
		toolDuration := time.Since(toolStart)

		if ts.hardAbortRequested() {
			exec.abortedByHardAbort = true
			return ToolControlBreak
		}

		if al.hooks != nil {
			toolResp, decision := al.hooks.AfterTool(turnCtx, &ToolResultHookResponse{
				Meta:      ts.eventMeta("runTurn", "turn.tool.after"),
				Context:   cloneTurnContext(ts.turnCtx),
				Tool:      toolName,
				Arguments: toolArgs,
				Result:    toolResult,
				Duration:  toolDuration,
			})
			switch decision.normalizedAction() {
			case HookActionContinue, HookActionModify:
				if toolResp != nil {
					if toolResp.Tool != "" {
						toolName = toolResp.Tool
					}
					if toolResp.Result != nil {
						toolResult = toolResp.Result
					}
				}
			case HookActionAbortTurn:
				exec.abortedByHook = true
				return ToolControlBreak
			case HookActionHardAbort:
				_ = ts.requestHardAbort()
				exec.abortedByHardAbort = true
				return ToolControlBreak
			}
		}

		if toolResult == nil {
			toolResult = tools.ErrorResult("hook 返回了 nil 工具结果")
		}

		// 工具执行完成后的 execution.report：非阻塞入队，失败只记日志。
		// 放在 AfterTool 之后，上报的 tool/result 反映模型最终看到的结果；
		// hook 主动中止回合时由 hook 自行决定，不在这里上报。
		al.reportToolExecution(ts, toolExecutionReport{
			ToolName:   toolName,
			ToolCallID: toolCallID,
			Arguments:  toolArgs,
			Result:     toolResult,
			StartedAt:  toolStart,
			Duration:   toolDuration,
			Iteration:  iteration,
		})

		if len(toolResult.Media) > 0 && toolResult.ResponseHandled {
			parts := make([]bus.MediaPart, 0, len(toolResult.Media))
			for _, ref := range toolResult.Media {
				part := bus.MediaPart{Ref: ref}
				if al.mediaStore != nil {
					if _, meta, err := al.mediaStore.ResolveWithMeta(ref); err == nil {
						part.Filename = meta.Filename
						part.ContentType = meta.ContentType
						part.Type = inferMediaType(meta.Filename, meta.ContentType)
					}
				}
				parts = append(parts, part)
			}
			outboundMedia := bus.OutboundMediaMessage{
				Channel: ts.channel,
				ChatID:  ts.chatID,
				Context: outboundContextFromInbound(
					ts.opts.Dispatch.InboundContext,
					ts.channel,
					ts.chatID,
					ts.opts.Dispatch.ReplyToMessageID(),
				),
				AgentID:    ts.agent.ID,
				SessionKey: ts.sessionKey,
				Scope:      outboundScopeFromSessionScope(ts.opts.Dispatch.SessionScope),
				Parts:      parts,
			}
			if al.channelManager != nil && ts.channel != "" && !constants.IsInternalChannel(ts.channel) {
				if err := al.channelManager.SendMedia(ctx, outboundMedia); err != nil {
					logger.WarnCF("agent", "Failed to deliver handled tool media",
						map[string]any{
							"agent_id": ts.agent.ID,
							"tool":     toolName,
							"channel":  ts.channel,
							"chat_id":  ts.chatID,
							"error":    err.Error(),
						})
					toolResult = tools.ErrorResult(fmt.Sprintf("附件交付失败：%v", err)).WithError(err)
				} else {
					handledAttachments = append(
						handledAttachments,
						buildProviderAttachments(al.mediaStore, toolResult.Media)...,
					)
				}
			} else if al.bus != nil {
				al.bus.PublishOutboundMedia(ctx, outboundMedia)
				toolResult.ResponseHandled = false
			}
		}

		if len(toolResult.Media) > 0 && !toolResult.ResponseHandled {
			toolResult.ArtifactTags = buildArtifactTags(al.mediaStore, toolResult.Media)
		}

		if !toolResult.ResponseHandled {
			exec.allResponsesHandled = false
		}

		shouldSendForUser := !toolResult.Silent &&
			toolResult.ForUser != "" &&
			(ts.opts.SendResponse || toolResult.ResponseHandled)
		if shouldSendForUser {
			al.bus.PublishOutbound(ctx, outboundMessageForTurn(ts, toolResult.ForUser))
			logger.DebugCF("agent", "Sent tool result to user",
				map[string]any{
					"tool":        toolName,
					"content_len": len(toolResult.ForUser),
				})
		}
		contentForLLM := toolResult.ContentForLLM()

		if al.cfg.Tools.IsFilterSensitiveDataEnabled() {
			contentForLLM = al.cfg.FilterSensitiveData(contentForLLM)
		}

		var toolResultMedia []string
		if len(toolResult.Media) > 0 && !toolResult.ResponseHandled {
			toolResultMedia = append(toolResultMedia, toolResult.Media...)
		}
		toolResultMsg := toolResultPromptMessage(contentForLLM, toolCallID, toolResultMedia)
		al.emitEvent(
			runtimeevents.KindAgentToolExecEnd,
			ts.eventMeta("runTurn", "turn.tool.end"),
			ToolExecEndPayload{
				Tool:       toolName,
				Duration:   toolDuration,
				ForLLMLen:  len(contentForLLM),
				ForUserLen: len(toolResult.ForUser),
				IsError:    toolResult.IsError,
				Async:      toolResult.Async,
			},
		)
		ts.recordToolExecution(
			toolName,
			!toolResult.IsError,
			toolErrorSummary(toolResult),
			inferSkillNamesFromToolCall(ts, toolName, toolArgs),
		)
		ts.recordContextTouch(toolName, toolArgs, toolResult.IsError)
		messages = append(messages, toolResultMsg)
		if !ts.opts.NoHistory {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, toolResultMsg)
			ts.recordPersistedMessage(toolResultMsg)
			ts.ingestMessage(turnCtx, al, toolResultMsg)
		}

		if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
			exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
		}

		skipReason := ""
		skipMessage := ""
		if len(exec.pendingMessages) > 0 {
			skipReason = "queued user steering message"
			skipMessage = "因排队中的用户消息而跳过。"
		} else if gracefulPending, _ := ts.gracefulInterruptRequested(); gracefulPending {
			skipReason = "graceful interrupt requested"
			skipMessage = "因请求了优雅中断而跳过。"
		}

		if skipReason != "" {
			remaining := len(normalizedToolCalls) - i - 1
			if remaining > 0 {
				logger.InfoCF("agent", "Turn checkpoint: skipping remaining tools",
					map[string]any{
						"agent_id":  ts.agent.ID,
						"completed": i + 1,
						"skipped":   remaining,
						"reason":    skipReason,
					})
				for j := i + 1; j < len(normalizedToolCalls); j++ {
					skippedTC := normalizedToolCalls[j]
					al.emitEvent(
						runtimeevents.KindAgentToolExecSkipped,
						ts.eventMeta("runTurn", "turn.tool.skipped"),
						ToolExecSkippedPayload{
							Tool:   skippedTC.Name,
							Reason: skipReason,
						},
					)
					skippedMsg := providers.Message{
						Role:       "tool",
						Content:    skipMessage,
						ToolCallID: skippedTC.ID,
					}
					messages = append(messages, skippedMsg)
					if !ts.opts.NoHistory {
						ts.agent.Sessions.AddFullMessage(ts.sessionKey, skippedMsg)
						ts.recordPersistedMessage(skippedMsg)
					}
				}
			}
			break toolLoop
		}

		if ts.pendingResults != nil {
			select {
			case result, ok := <-ts.pendingResults:
				if ok && result != nil && result.ForLLM != "" {
					content := al.cfg.FilterSensitiveData(result.ForLLM)
					msg := subTurnResultPromptMessage(content)
					messages = append(messages, msg)
					if !ts.opts.NoHistory {
						ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
					}
				}
			default:
			}
		}
	}

	exec.messages = messages

	// 若存在待处理 steering 则继续（无论 allResponsesHandled 是什么值）。
	// 这涵盖以下情况：工具因 steering 被部分执行、部分跳过，
	// 但其中某个工具 ResponseHandled=false（于是 allResponsesHandled=false）。
	if len(exec.pendingMessages) > 0 {
		logger.InfoCF("agent", "Pending steering after partial tool execution; continuing turn",
			map[string]any{
				"agent_id":            ts.agent.ID,
				"pending_count":       len(exec.pendingMessages),
				"allResponsesHandled": exec.allResponsesHandled,
			})
		exec.allResponsesHandled = false
		return ToolControlContinue
	}

	// 轮询新到达的 steering 消息
	if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
		logger.InfoCF("agent", "Steering arrived after tool delivery; continuing turn",
			map[string]any{
				"agent_id":       ts.agent.ID,
				"steering_count": len(steerMsgs),
			})
		exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
		exec.allResponsesHandled = false
		return ToolControlContinue
	}

	// 没有待处理 steering：根据 allResponsesHandled 决定 finalize 还是 break
	if exec.allResponsesHandled {
		summaryMsg := providers.Message{
			Role:        "assistant",
			Content:     handledToolResponseSummary,
			Attachments: append([]providers.Attachment(nil), handledAttachments...),
		}
		if !ts.opts.NoHistory {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, summaryMsg)
			ts.recordPersistedMessage(summaryMsg)
			ts.ingestMessage(turnCtx, al, summaryMsg)
			if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
				logger.ErrorCF("agent", "Failed to save session after tool delivery",
					map[string]any{
						"agent_id":    ts.agent.ID,
						"session_key": ts.sessionKey,
						"error":       err.Error(),
					})
				al.emitEvent(
					runtimeevents.KindAgentError,
					ts.eventMeta("runTurn", "turn.session.save"),
					ErrorPayload{Stage: "session_save", Message: err.Error()},
				)
			}
		}
		if !ts.opts.NoHistory && ts.opts.EnableSummary {
			al.contextManager.Compact(turnCtx, &CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonSummarize,
				Budget:     ts.agent.ContextWindow,
			})
		}
		ts.setPhase(TurnPhaseCompleted)
		ts.setFinalContent("")
		if al.channelManager != nil && ts.channel != "" {
			al.channelManager.DismissToolFeedback(ctx, ts.channel, ts.chatID, ts.opts.InboundContext)
		}
		logger.InfoCF("agent", "Tool output satisfied delivery; ending turn without follow-up LLM",
			map[string]any{
				"agent_id":   ts.agent.ID,
				"iteration":  iteration,
				"tool_count": len(normalizedToolCalls),
			})
		return ToolControlBreak
	}

	// allResponsesHandled=false 且没有待处理 steering：继续执行，以便协调器
	// 再发起一次 LLM 调用。工具结果已在 messages 中，LLM 会在下一次迭代
	// 将其作为 finalContent 返回。
	ts.agent.Tools.TickTTL()
	logger.DebugCF("agent", "TTL tick after tool execution", map[string]any{
		"agent_id": ts.agent.ID, "iteration": iteration,
	})
	return ToolControlContinue
}

// -----------------------------------------------------------------------------
// 并发工具批次路径（DSH 并行工具调度）
// -----------------------------------------------------------------------------
//
// 该路径是 DSH executeToolBatch 语义的保守移植：
// 独占调用充当屏障，连续的并行安全调用共享滚动并发池，
// 派发可能重叠但结果按模型顺序提交，中止会给每个未派发调用留下合成的工具结果
// （因此工具调用绝不会缺少工具结果）。
//
// 只有在以下条件全部满足时才接管：
//   1. 未挂载任何工具 hook（目前 BeforeTool/ApproveTool/AfterTool 是串行拦截的；
//      并行派发会改变 hook 状态顺序）；
//   2. 模型要求至少两个调用，且至少存在一对相邻的并行安全调用（单调用回合保持串行路径）；
//   3. 入口处没有排队 steering 消息（用户 steering 必须优先于推测性并行工作）。
// 否则 ExecuteTools 保持原有串行循环不变。当并发路径不满足条件，或无法保持 hook 语义时，
// 回退行为与既有串行行为完全一致。

// toolBatchParallelism 是并行安全调用滚动池的固定大小。
// 不新增外部配置：批次路径对符合条件的回合是默认行为，
// 否则静默回退。
const toolBatchParallelism = toolBatchDefaultParallelism

// batchedToolPlan 是经过非执行型预检查（turn-profile 允许/拒绝）后的一个模型工具调用。
// 被拒绝的调用保留预构建的工具结果消息，以便在模型位置提交。
type batchedToolPlan struct {
	index         int
	call          providers.ToolCall
	toolName      string
	toolArgs      map[string]any
	deniedMessage *providers.Message
}

// shouldUseConcurrentToolBatch 决定并发路径是否可以接管而不改变可观察语义。
func (p *Pipeline) shouldUseConcurrentToolBatch(ts *turnState, exec *turnExecution) bool {
	if p == nil || p.al == nil || ts == nil || exec == nil {
		return false
	}
	if p.toolHooksWouldIntercept() {
		return false
	}
	calls := exec.normalizedToolCalls
	if len(calls) < 2 {
		return false
	}
	if p.al.pendingSteeringCountForScope(ts.sessionKey) > 0 {
		return false
	}
	previousSafe := false
	for _, call := range calls {
		safe := p.toolCallBatchSafe(ts, call.Name)
		if safe && previousSafe {
			return true
		}
		previousSafe = safe
	}
	return false
}

// toolHooksWouldIntercept 报告当前是否挂载了任何 hook。
//
// 存在 hook 时批次路径被刻意禁用：串行循环会把 BeforeTool / ApproveTool / AfterTool 与执行交错，
// hook 状态可能依赖该顺序。读取 hook 管理器的有序列表是只读的，并由其互斥锁保护；
// 这里不修改任何 hook 代码。
func (p *Pipeline) toolHooksWouldIntercept() bool {
	if p == nil || p.al == nil {
		return false
	}
	manager := p.al.hooks
	if manager == nil {
		return false
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return len(manager.ordered) > 0
}

// toolCallBatchSafe 报告单个调用是否可以并发派发。
// 已注册工具使用显式的 ConcurrencySafe 能力（若存在），
// 并回退到 pkg/tools 中的名称级允许列表。未知工具保持独占，
// 使缺失工具错误保持其串行位置。
func (p *Pipeline) toolCallBatchSafe(ts *turnState, toolName string) bool {
	if ts == nil || ts.agent == nil || ts.agent.Tools == nil {
		return false
	}
	tool, ok := ts.agent.Tools.Get(toolName)
	if !ok {
		return false
	}
	return tools.IsToolParallelSafe(tool)
}

// executeToolsConcurrentBatch 是无 hook、无 steering 场景下串行工具循环的并发版本。它保持：
//   - turn-profile 拒绝消息处于其模型位置；
//   - 工具 start/end/skipped 事件与工具反馈；
//   - 媒体投递、provider 附件与 ResponseHandled 语义；
//   - 除非 NoHistory，否则保持会话持久化；
//   - recordToolExecution / recordContextTouch；
//   - 每条已提交结果之后的 steering 与待处理结果轮询；
//   - allResponsesHandled 的摘要/中断尾部。
func (p *Pipeline) executeToolsConcurrentBatch(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) ToolControl {
	al := p.al
	calls := exec.normalizedToolCalls
	messages := exec.messages

	if ts.hardAbortRequested() {
		exec.abortedByHardAbort = true
		return ToolControlBreak
	}

	// 阶段 1：按模型顺序执行非执行型预检查（turn profile）。
	plan := make([]batchedToolPlan, 0, len(calls))
	for index, call := range calls {
		item := batchedToolPlan{
			index:    index,
			call:     call,
			toolName: call.Name,
			toolArgs: cloneStringAnyMap(call.Arguments),
		}
		if !turnProfileToolAllowed(ts.profile, item.toolName) {
			exec.allResponsesHandled = false
			denyContent := fmt.Sprintf("工具 %q 不在当前回合配置允许范围内。", item.toolName)
			al.emitEvent(
				runtimeevents.KindAgentToolExecSkipped,
				ts.eventMeta("runTurn", "turn.tool.skipped"),
				ToolExecSkippedPayload{
					Tool:   item.toolName,
					Reason: denyContent,
				},
			)
			denied := providers.Message{
				Role:       "tool",
				Content:    denyContent,
				ToolCallID: call.ID,
			}
			item.deniedMessage = &denied
		} else {
			// 循环卫生：在执行前计数尝试（与串行路径规则相同；被拒绝的调用不计数）。
			ts.observeToolCallForLoopGuard(item.toolName, item.toolArgs)
		}
		plan = append(plan, item)
	}

	// 阶段 2：只用已批准的调用组装批次。
	batchIndexByPlan := make([]int, len(plan))
	for i := range batchIndexByPlan {
		batchIndexByPlan[i] = -1
	}
	batchItems := make([]toolBatchItem, 0, len(plan))
	for i := range plan {
		if plan[i].deniedMessage != nil {
			continue
		}
		batchIndexByPlan[i] = len(batchItems)
		batchItems = append(batchItems, toolBatchItem{
			Index: plan[i].index,
			Tool:  plan[i].toolName,
			Args:  plan[i].toolArgs,
		})
	}

	messagesAtDispatch := messages
	execFn := func(
		callCtx context.Context,
		index int,
		_ string,
		toolArgs map[string]any,
	) (*tools.ToolResult, error) {
		call := plan[index].call
		return p.executeToolCallForBatch(
			callCtx,
			ts,
			exec,
			iteration,
			call,
			toolArgs,
			messagesAtDispatch,
		), nil
	}

	// 分组与准入必须使用同一个并行安全判定：toolCallBatchSafe 会先按名称从注册表
	// 取出工具再判断 ConcurrencySafe 能力，未注册/未知工具返回 false（保守串行），
	// 避免“名称在允许列表但实现声明不可并行”的工具被错误并行。
	isSafe := func(toolName string) bool {
		return p.toolCallBatchSafe(ts, toolName)
	}
	outcomes := runToolBatch(turnCtx, batchItems, execFn, toolBatchParallelism, nil, isSafe)

	// 阶段 3：按模型顺序提交结果（绝不按派发顺序）。
	handledAttachments := make([]providers.Attachment, 0)
	for i := range plan {
		if ts.hardAbortRequested() {
			exec.abortedByHardAbort = true
			return ToolControlBreak
		}
		item := &plan[i]
		if item.deniedMessage != nil {
			messages = append(messages, *item.deniedMessage)
			if !ts.opts.NoHistory {
				ts.agent.Sessions.AddFullMessage(ts.sessionKey, *item.deniedMessage)
				ts.recordPersistedMessage(*item.deniedMessage)
			}
			continue
		}
		batchPos := batchIndexByPlan[i]
		if batchPos < 0 || batchPos >= len(outcomes) {
			continue
		}
		outcome := outcomes[batchPos]
		if outcome.Skipped {
			reason := strings.TrimSpace(outcome.SkipReason)
			if reason == "" {
				reason = toolBatchAbortedBeforeDispatch
			}
			al.emitEvent(
				runtimeevents.KindAgentToolExecSkipped,
				ts.eventMeta("runTurn", "turn.tool.skipped"),
				ToolExecSkippedPayload{
					Tool:   item.toolName,
					Reason: reason,
				},
			)
			skippedMsg := providers.Message{
				Role:       "tool",
				Content:    reason,
				ToolCallID: item.call.ID,
			}
			messages = append(messages, skippedMsg)
			if !ts.opts.NoHistory {
				ts.agent.Sessions.AddFullMessage(ts.sessionKey, skippedMsg)
				ts.recordPersistedMessage(skippedMsg)
			}
			exec.allResponsesHandled = false
			continue
		}

		toolResult := outcome.Result
		if toolResult == nil {
			toolResult = tools.ErrorResult("工具返回了 nil 结果")
		}
		toolName := item.toolName
		toolArgs := item.toolArgs

		// 并发批次路径的 execution.report：与串行路径同一入口、同样非阻塞。
		// outcome 带实际执行时间窗；被跳过的调用在上面 continue 掉，不会上报。
		al.reportToolExecution(ts, toolExecutionReport{
			ToolName:   toolName,
			ToolCallID: item.call.ID,
			Arguments:  toolArgs,
			Result:     toolResult,
			StartedAt:  outcome.StartedAt,
			Duration:   outcome.Duration,
			Iteration:  iteration,
		})

		if len(toolResult.Media) > 0 && toolResult.ResponseHandled {
			parts := make([]bus.MediaPart, 0, len(toolResult.Media))
			for _, ref := range toolResult.Media {
				part := bus.MediaPart{Ref: ref}
				if al.mediaStore != nil {
					if _, meta, err := al.mediaStore.ResolveWithMeta(ref); err == nil {
						part.Filename = meta.Filename
						part.ContentType = meta.ContentType
						part.Type = inferMediaType(meta.Filename, meta.ContentType)
					}
				}
				parts = append(parts, part)
			}
			outboundMedia := bus.OutboundMediaMessage{
				Channel: ts.channel,
				ChatID:  ts.chatID,
				Context: outboundContextFromInbound(
					ts.opts.Dispatch.InboundContext,
					ts.channel,
					ts.chatID,
					ts.opts.Dispatch.ReplyToMessageID(),
				),
				AgentID:    ts.agent.ID,
				SessionKey: ts.sessionKey,
				Scope:      outboundScopeFromSessionScope(ts.opts.Dispatch.SessionScope),
				Parts:      parts,
			}
			if al.channelManager != nil && ts.channel != "" && !constants.IsInternalChannel(ts.channel) {
				if err := al.channelManager.SendMedia(ctx, outboundMedia); err != nil {
					logger.WarnCF("agent", "Failed to deliver handled tool media",
						map[string]any{
							"agent_id": ts.agent.ID,
							"tool":     toolName,
							"channel":  ts.channel,
							"chat_id":  ts.chatID,
							"error":    err.Error(),
						})
					toolResult = tools.ErrorResult(fmt.Sprintf("附件交付失败：%v", err)).WithError(err)
				} else {
					handledAttachments = append(
						handledAttachments,
						buildProviderAttachments(al.mediaStore, toolResult.Media)...,
					)
				}
			} else if al.bus != nil {
				al.bus.PublishOutboundMedia(ctx, outboundMedia)
				toolResult.ResponseHandled = false
			}
		}

		if len(toolResult.Media) > 0 && !toolResult.ResponseHandled {
			toolResult.ArtifactTags = buildArtifactTags(al.mediaStore, toolResult.Media)
		}

		if !toolResult.ResponseHandled {
			exec.allResponsesHandled = false
		}

		shouldSendForUser := !toolResult.Silent &&
			toolResult.ForUser != "" &&
			(ts.opts.SendResponse || toolResult.ResponseHandled)
		if shouldSendForUser {
			al.bus.PublishOutbound(ctx, outboundMessageForTurn(ts, toolResult.ForUser))
			logger.DebugCF("agent", "Sent tool result to user",
				map[string]any{
					"tool":        toolName,
					"content_len": len(toolResult.ForUser),
				})
		}

		contentForLLM := toolResult.ContentForLLM()
		if al.cfg.Tools.IsFilterSensitiveDataEnabled() {
			contentForLLM = al.cfg.FilterSensitiveData(contentForLLM)
		}

		var toolResultMedia []string
		if len(toolResult.Media) > 0 && !toolResult.ResponseHandled {
			toolResultMedia = append(toolResultMedia, toolResult.Media...)
		}
		toolResultMsg := toolResultPromptMessage(contentForLLM, item.call.ID, toolResultMedia)
		al.emitEvent(
			runtimeevents.KindAgentToolExecEnd,
			ts.eventMeta("runTurn", "turn.tool.end"),
			ToolExecEndPayload{
				Tool:       toolName,
				ForLLMLen:  len(contentForLLM),
				ForUserLen: len(toolResult.ForUser),
				IsError:    toolResult.IsError,
				Async:      toolResult.Async,
			},
		)
		ts.recordToolExecution(
			toolName,
			!toolResult.IsError,
			toolErrorSummary(toolResult),
			inferSkillNamesFromToolCall(ts, toolName, toolArgs),
		)
		ts.recordContextTouch(toolName, toolArgs, toolResult.IsError)
		messages = append(messages, toolResultMsg)
		if !ts.opts.NoHistory {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, toolResultMsg)
			ts.recordPersistedMessage(toolResultMsg)
			ts.ingestMessage(turnCtx, al, toolResultMsg)
		}

		if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
			exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
		}

		if ts.pendingResults != nil {
			select {
			case result, ok := <-ts.pendingResults:
				if ok && result != nil && result.ForLLM != "" {
					content := al.cfg.FilterSensitiveData(result.ForLLM)
					msg := subTurnResultPromptMessage(content)
					messages = append(messages, msg)
					if !ts.opts.NoHistory {
						ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
					}
				}
			default:
			}
		}
	}

	exec.messages = messages

	// 若存在待处理 steering 则继续（与串行尾部一致）。
	if len(exec.pendingMessages) > 0 {
		logger.InfoCF("agent", "Pending steering after concurrent tool batch; continuing turn",
			map[string]any{
				"agent_id":            ts.agent.ID,
				"pending_count":       len(exec.pendingMessages),
				"allResponsesHandled": exec.allResponsesHandled,
			})
		exec.allResponsesHandled = false
		return ToolControlContinue
	}

	if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
		logger.InfoCF("agent", "Steering arrived after concurrent tool batch; continuing turn",
			map[string]any{
				"agent_id":       ts.agent.ID,
				"steering_count": len(steerMsgs),
			})
		exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
		exec.allResponsesHandled = false
		return ToolControlContinue
	}

	if exec.allResponsesHandled {
		summaryMsg := providers.Message{
			Role:        "assistant",
			Content:     handledToolResponseSummary,
			Attachments: append([]providers.Attachment(nil), handledAttachments...),
		}
		if !ts.opts.NoHistory {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, summaryMsg)
			ts.recordPersistedMessage(summaryMsg)
			ts.ingestMessage(turnCtx, al, summaryMsg)
			if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
				logger.ErrorCF("agent", "Failed to save session after tool delivery",
					map[string]any{
						"agent_id":    ts.agent.ID,
						"session_key": ts.sessionKey,
						"error":       err.Error(),
					})
				al.emitEvent(
					runtimeevents.KindAgentError,
					ts.eventMeta("runTurn", "turn.session.save"),
					ErrorPayload{Stage: "session_save", Message: err.Error()},
				)
			}
		}
		if !ts.opts.NoHistory && ts.opts.EnableSummary {
			al.contextManager.Compact(turnCtx, &CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonSummarize,
				Budget:     ts.agent.ContextWindow,
			})
		}
		ts.setPhase(TurnPhaseCompleted)
		ts.setFinalContent("")
		if al.channelManager != nil && ts.channel != "" {
			al.channelManager.DismissToolFeedback(ctx, ts.channel, ts.chatID, ts.opts.InboundContext)
		}
		logger.InfoCF("agent", "Tool output satisfied delivery; ending turn without follow-up LLM",
			map[string]any{
				"agent_id":   ts.agent.ID,
				"iteration":  iteration,
				"tool_count": len(calls),
				"batch":      true,
			})
		return ToolControlBreak
	}

	ts.agent.Tools.TickTTL()
	logger.DebugCF("agent", "TTL tick after concurrent tool batch", map[string]any{
		"agent_id": ts.agent.ID, "iteration": iteration,
	})
	return ToolControlContinue
}

// executeToolCallForBatch 执行串行循环在执行前立即完成的每次调用副作用
// （日志、start 事件、工具反馈），然后用相同的上下文接线运行工具。
func (p *Pipeline) executeToolCallForBatch(
	callCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
	call providers.ToolCall,
	toolArgs map[string]any,
	messages []providers.Message,
) *tools.ToolResult {
	al := p.al
	toolName := call.Name

	argsJSON, _ := json.Marshal(toolArgs)
	argsPreview := utils.Truncate(string(argsJSON), 200)
	logger.InfoCF("agent", fmt.Sprintf("Tool call (batch): %s(%s)", toolName, argsPreview),
		map[string]any{
			"agent_id":  ts.agent.ID,
			"tool":      toolName,
			"iteration": iteration,
		})
	al.emitEvent(
		runtimeevents.KindAgentToolExecStart,
		ts.eventMeta("runTurn", "turn.tool.start"),
		ToolExecStartPayload{
			Tool:      toolName,
			Arguments: cloneEventArguments(toolArgs),
		},
	)

	if shouldPublishToolFeedback(al.cfg, ts) && ts.channel != "pico" {
		toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
			exec.response,
			call,
			messages,
		)
		feedbackMsg := utils.FormatToolFeedbackMessage(
			toolName,
			toolFeedbackExplanation,
			toolFeedbackArgsPreview(toolArgs, 0),
		)
		fbCtx, fbCancel := context.WithTimeout(callCtx, 3*time.Second)
		_ = al.bus.PublishOutbound(fbCtx, outboundMessageForTurnWithOptions(
			ts,
			feedbackMsg,
			outboundTurnMessageOptions{kind: messageKindToolFeedback},
		))
		fbCancel()
	}

	asyncCallback := p.batchAsyncCallback(ts, iteration, toolName)
	execCtx := tools.WithToolInboundContext(
		callCtx,
		ts.channel,
		ts.chatID,
		ts.opts.Dispatch.MessageID(),
		ts.opts.Dispatch.ReplyToMessageID(),
	)
	execCtx = tools.WithToolSessionContext(
		execCtx,
		ts.agent.ID,
		ts.sessionKey,
		ts.opts.Dispatch.SessionScope,
	)
	execCtx = integrationtools.WithTurnSource(execCtx, turnSourceFromString(ts.opts.TurnSource))
	return ts.agent.Tools.ExecuteWithContext(
		execCtx,
		toolName,
		toolArgs,
		ts.channel,
		ts.chatID,
		asyncCallback,
	)
}

// batchAsyncCallback 对应串行异步工具回调：面向用户的文本立即发布，
// 面向 LLM 的内容异步重新注入。
func (p *Pipeline) batchAsyncCallback(
	ts *turnState,
	iteration int,
	toolName string,
) tools.AsyncCallback {
	al := p.al
	return func(_ context.Context, result *tools.ToolResult) {
		if !result.Silent && result.ForUser != "" {
			outCtx, outCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer outCancel()
			_ = al.bus.PublishOutbound(outCtx, outboundMessageForTurn(ts, result.ForUser))
		}

		content := result.ContentForLLM()
		if content == "" {
			return
		}

		content = al.cfg.FilterSensitiveData(content)

		logger.InfoCF("agent", "Async tool completed, publishing result",
			map[string]any{
				"tool":        toolName,
				"content_len": len(content),
				"channel":     ts.channel,
			})
		al.emitEvent(
			runtimeevents.KindAgentFollowUpQueued,
			ts.scope.meta(iteration, "runTurn", "turn.follow_up.queued"),
			FollowUpQueuedPayload{
				SourceTool: toolName,
				ContentLen: len(content),
			},
		)
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		_ = al.bus.PublishInbound(pubCtx, bus.InboundMessage{
			Context: bus.InboundContext{
				Channel:  "system",
				ChatID:   fmt.Sprintf("%s:%s", ts.channel, ts.chatID),
				ChatType: "direct",
				SenderID: fmt.Sprintf("async:%s", toolName),
			},
			Content: content,
		})
	}
}
