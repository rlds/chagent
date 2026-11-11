// Chagent - 超轻量个人 AI 智能体

package agent

import (
	"context"
	"strings"

	"github.com/rlds/chagent/pkg/bus"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
)

// Finalize 处理轮次收尾，分为两种情形：
// - 当 allResponsesHandled=true 时提前返回（ExecuteTools 已完成收尾）
// - allResponsesHandled=false 时进行常规收尾（设置 finalContent、保存会话、压缩）
func (p *Pipeline) Finalize(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	turnStatus TurnEndStatus,
	finalContent string,
) (turnResult, error) {
	al := p.al

	// 当 allResponsesHandled=true 时，ExecuteTools 已完成收尾
	// （追加 handledToolResponseSummary、保存会话、将阶段设为 Completed）。
	// 但仍需检查硬中止——若请求了硬中止，则中止该轮次。
	if exec.allResponsesHandled {
		if ts.hardAbortRequested() {
			return al.abortTurn(ts)
		}
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			modelName:    exec.llmModelName,
			usage:        execResponseUsage(exec.response),
			status:       turnStatus,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, nil
	}

	ts.setPhase(TurnPhaseFinalizing)
	ts.setFinalContent(finalContent)
	if !ts.opts.NoHistory {
		// 迭代上限检查点可以在没有最终答案的情况下收尾（自主驱动会在下一轮继续）。
		// 不要持久化空的 assistant 消息：某些 provider 拒绝空内容，
		// 而且它会污染重放的历史。
		finalMsg := providers.Message{
			Role:             "assistant",
			Content:          finalContent,
			ModelName:        exec.llmModelName,
			ReasoningContent: responseReasoningContent(exec.response),
		}
		if strings.TrimSpace(finalMsg.Content) != "" || strings.TrimSpace(finalMsg.ReasoningContent) != "" {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, finalMsg)
			ts.recordPersistedMessage(finalMsg)
			ts.ingestMessage(turnCtx, al, finalMsg)
		}
		if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
			al.emitEvent(
				runtimeevents.KindAgentError,
				ts.eventMeta("runTurn", "turn.error"),
				ErrorPayload{
					Stage:   "session_save",
					Message: err.Error(),
				},
			)
			cancelConfiguredStreamingLLM(turnCtx, exec)
			return turnResult{status: TurnEndStatusError}, err
		}
	}

	if !ts.opts.NoHistory && ts.opts.EnableSummary {
		al.contextManager.Compact(
			turnCtx,
			&CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonSummarize,
				Budget:     ts.agent.ContextWindow,
			},
		)
	}

	contextUsage := computeContextUsage(ts.agent, ts.sessionKey)
	// 将 LLM 用量传给流式发布器，用于最终投递
	if exec.streamingPublisher != nil {
		exec.streamingPublisher.usage = execResponseUsage(exec.response)
	}
	streamErr := finalizeConfiguredStreamingLLM(turnCtx, ts, exec, finalContent, contextUsage)
	// 如果流式输出从未变得可见，则沿用旧版 Pico 临时发布路径，
	// 以便最终答案仍能在常规 SendResponse 之外投递。
	if ((streamErr != nil && !isConfiguredStreamingVisibleError(streamErr)) || exec.streamingFallback) &&
		!ts.opts.SendResponse && ts.opts.AllowInterimPicoPublish && finalContent != "" {
		msg := outboundMessageForTurnWithOptions(ts, finalContent, outboundTurnMessageOptions{
			modelName: exec.llmModelName,
		})
		msg.ContextUsage = contextUsage
		markFinalOutbound(&msg)
		_ = al.bus.PublishOutbound(turnCtx, msg)
	}
	if streamErr != nil && isConfiguredStreamingVisibleError(streamErr) {
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			status:       TurnEndStatusError,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, streamErr
	}
	ts.setPhase(TurnPhaseCompleted)
	return turnResult{
		finalContent: finalContent,
		modelName:    exec.llmModelName,
		usage:        execResponseUsage(exec.response),
		status:       turnStatus,
		followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
	}, nil
}
