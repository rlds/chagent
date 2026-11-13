// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools"
	"github.com/rlds/chagent/pkg/utils"
)

func (al *AgentLoop) maybePublishError(ctx context.Context, channel, chatID, sessionKey string, err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	al.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, fmt.Sprintf("处理消息时出错：%v", err), "", nil)
	return true
}

func (al *AgentLoop) publishResponseOrError(
	ctx context.Context,
	channel, chatID, sessionKey string,
	response string,
	modelName string,
	usage *providers.UsageInfo,
	err error,
) {
	if err != nil {
		if !al.maybePublishError(ctx, channel, chatID, sessionKey, err) {
			return
		}
		response = ""
	}
	al.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, response, modelName, usage)
}

func (al *AgentLoop) PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string, modelName string, usage *providers.UsageInfo) {
	if response == "" {
		return
	}

	alreadySentToSameChat := false
	defaultAgent := al.GetRegistry().GetDefaultAgent()
	if defaultAgent != nil {
		if tool, ok := defaultAgent.Tools.Get("message"); ok {
			if mt, ok := tool.(*tools.MessageTool); ok {
				alreadySentToSameChat = mt.HasSentTo(sessionKey, channel, chatID)
			}
		}
	}

	if alreadySentToSameChat {
		if al.channelManager != nil && channel != "" && chatID != "" {
			dismissCtx, dismissCancel := context.WithTimeout(ctx, 5*time.Second)
			al.channelManager.DismissToolFeedback(
				dismissCtx,
				channel,
				chatID,
				nil,
			)
			dismissCancel()
		}
		logger.DebugCF(
			"agent",
			"Skipped outbound (message tool already sent to same chat)",
			map[string]any{"channel": channel, "chat_id": chatID},
		)
		return
	}

	msg := bus.OutboundMessage{
		Context:    bus.NewOutboundContext(channel, chatID, ""),
		SessionKey: sessionKey,
		Content:    response,
		Usage:      usage,
	}
	if sessionKey != "" {
		msg.ContextUsage = computeContextUsage(al.agentForSession(sessionKey), sessionKey)
	}
	if trimmedModelName := strings.TrimSpace(modelName); trimmedModelName != "" {
		if msg.Context.Raw == nil {
			msg.Context.Raw = make(map[string]string, 1)
		}
		msg.Context.Raw["model_name"] = trimmedModelName
	}
	markFinalOutbound(&msg)
	al.bus.PublishOutbound(ctx, msg)
	logger.InfoCF("agent", "Published outbound response",
		map[string]any{
			"channel":     channel,
			"chat_id":     chatID,
			"content_len": len(response),
			"model_name":  modelName,
			"has_usage":   usage != nil,
		})
}

func (al *AgentLoop) targetReasoningChannelID(channelName string) (chatID string) {
	if al.channelManager == nil {
		return ""
	}
	if ch, ok := al.channelManager.GetChannel(channelName); ok {
		return ch.ReasoningChannelID()
	}
	return ""
}

func (al *AgentLoop) publishPicoReasoning(
	ctx context.Context,
	reasoningContent, chatID, sessionKey, modelName string,
	usage *providers.UsageInfo,
) {
	if reasoningContent == "" || chatID == "" {
		return
	}

	if ctx.Err() != nil {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	raw := map[string]string{metadataKeyMessageKind: messageKindThought}
	if trimmedModelName := strings.TrimSpace(modelName); trimmedModelName != "" {
		raw["model_name"] = trimmedModelName
	}

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.InboundContext{
			Channel: "pico",
			ChatID:  chatID,
			Raw:     raw,
		},
		SessionKey: sessionKey,
		Content:    reasoningContent,
		Usage:      usage,
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Pico reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish pico reasoning (best-effort)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		}
	}
}

func (al *AgentLoop) publishPicoToolCallInterim(
	ctx context.Context,
	ts *turnState,
	modelName string,
	reasoningContent string,
	content string,
	toolCalls []providers.ToolCall,
) {
	if ts == nil || ts.chatID == "" || al == nil || al.bus == nil {
		return
	}

	if strings.TrimSpace(reasoningContent) != "" {
		pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
		err := al.bus.PublishOutbound(
			pubCtx,
			outboundMessageForTurnWithOptions(
				ts,
				reasoningContent,
				outboundTurnMessageOptions{
					kind:      messageKindThought,
					modelName: modelName,
				},
			),
		)
		pubCancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, bus.ErrBusClosed) {
			logger.WarnCF("agent", "Failed to publish pico reasoning", map[string]any{
				"channel": ts.channel,
				"chat_id": ts.chatID,
				"error":   err.Error(),
			})
		}
	}

	if !ts.opts.AllowInterimPicoPublish {
		return
	}

	// tool_calls 是任务详情/对接接口的事实数据，必须完整上报参数；
	// 不得复用 tool_feedback 的预览限制，否则长参数会在任务执行过程中被截断。
	visibleToolCalls := utils.BuildVisibleToolCalls(toolCalls, 0)
	duplicateToolCallContent := len(visibleToolCalls) > 0 &&
		utils.ToolCallExplanationDuplicatesContent(content, toolCalls)

	if strings.TrimSpace(content) != "" && !duplicateToolCallContent {
		pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
		err := al.bus.PublishOutbound(
			pubCtx,
			outboundMessageForTurnWithOptions(ts, content, outboundTurnMessageOptions{
				modelName: modelName,
			}),
		)
		pubCancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, bus.ErrBusClosed) {
			logger.WarnCF("agent", "Failed to publish pico interim assistant content", map[string]any{
				"channel": ts.channel,
				"chat_id": ts.chatID,
				"error":   err.Error(),
			})
		}
	}

	if len(visibleToolCalls) == 0 {
		return
	}

	rawToolCalls, err := json.Marshal(visibleToolCalls)
	if err != nil {
		logger.WarnCF("agent", "Failed to serialize pico tool calls", map[string]any{
			"channel": ts.channel,
			"chat_id": ts.chatID,
			"error":   err.Error(),
		})
		return
	}

	msg := outboundMessageForTurnWithOptions(ts, "", outboundTurnMessageOptions{
		kind:      messageKindToolCalls,
		modelName: modelName,
		raw: map[string]string{
			metadataKeyToolCalls: string(rawToolCalls),
		},
	})

	pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
	err = al.bus.PublishOutbound(pubCtx, msg)
	pubCancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, bus.ErrBusClosed) {
		logger.WarnCF("agent", "Failed to publish pico tool calls", map[string]any{
			"channel": ts.channel,
			"chat_id": ts.chatID,
			"error":   err.Error(),
		})
	}
}

func (al *AgentLoop) handleReasoning(
	ctx context.Context,
	reasoningContent, channelName, channelID string,
) {
	if reasoningContent == "" || channelName == "" || channelID == "" {
		return
	}

	// 在尝试发布前先检查 context 是否已取消，
	// 因为 PublishOutbound 的 select 可能在发送与 ctx.Done() 之间产生竞态。
	if ctx.Err() != nil {
		return
	}

	// 使用较短的超时，避免出站总线已满时 goroutine 无限阻塞。
	// 推理输出属于尽力而为的结果；为避免 goroutine 堆积，丢弃它是可以接受的。
	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.NewOutboundContext(channelName, channelID, ""),
		Content: reasoningContent,
	}); err != nil {
		// 将 context.DeadlineExceeded / context.Canceled 视为预期情况
		// （负载下总线已满，或父 context 被取消）。这里检查错误本身
		// 而不是 ctx.Err()，因为父 ctx 仍存活时 pubCtx 也可能超时（5 秒）。
		// 同样将 ErrBusClosed 视为预期情况 —— 它出现在正常关闭过程中，
		// 即总线在所有 goroutine 结束前就被关闭。
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish reasoning (best-effort)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		}
	}
}
