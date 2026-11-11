// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"context"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/logger"
)

func (al *AgentLoop) processMessageSync(ctx context.Context, msg bus.InboundMessage) {
	if al.channelManager != nil {
		defer al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
	}

	response, modelName, usage, err := al.processMessage(ctx, msg)
	al.publishResponseOrError(ctx, msg.Channel, msg.ChatID, msg.SessionKey, response, modelName, usage, err)
}

func (al *AgentLoop) runTurnWithSteering(ctx context.Context, initialMsg bus.InboundMessage) {
	// 处理初始消息
	response, modelName, usage, err := al.processMessage(ctx, initialMsg)
	if err != nil {
		if !al.maybePublishError(ctx, initialMsg.Channel, initialMsg.ChatID, initialMsg.SessionKey, err) {
			return // 上下文已取消
		}
		response = ""
	}
	finalResponse := response
	finalModelName := modelName
	finalUsage := usage

	// 构建续跑目标
	target, targetErr := al.buildContinuationTarget(initialMsg)
	if targetErr != nil {
		logger.WarnCF("agent", "Failed to build steering continuation target",
			map[string]any{
				"channel": initialMsg.Channel,
				"error":   targetErr.Error(),
			})
		return
	}
	if target == nil {
		// 系统消息或不可路由，响应已发布
		return
	}

	continued, continueErr := al.drainQueuedSteeringContinuations(ctx, target)
	if continueErr != nil {
		logger.WarnCF("agent", "Failed to continue queued steering",
			map[string]any{
				"channel": target.Channel,
				"chat_id": target.ChatID,
				"error":   continueErr.Error(),
			})
	} else if continued != "" {
		finalResponse = continued
	}

	// 发布最终响应
	if finalResponse != "" {
		al.PublishResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, finalResponse, finalModelName, finalUsage)
	}
}

func (al *AgentLoop) drainQueuedSteeringContinuations(
	ctx context.Context,
	target *continuationTarget,
) (string, error) {
	if target == nil {
		return "", nil
	}

	finalResponse := ""
	for al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		if err := ctx.Err(); err != nil {
			return finalResponse, err
		}

		logger.InfoCF("agent", "Continuing queued steering after turn end",
			map[string]any{
				"channel":     target.Channel,
				"chat_id":     target.ChatID,
				"session_key": target.SessionKey,
				"queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
			})

		continued, continueErr := al.Continue(ctx, target.SessionKey, target.Channel, target.ChatID)
		if continueErr != nil {
			return finalResponse, continueErr
		}
		if continued == "" {
			break
		}
		finalResponse = continued
	}

	return finalResponse, nil
}

func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
	if msg.Channel == "system" {
		return "", "", false
	}

	route, agent, err := al.resolveMessageRoute(msg)
	if err != nil || agent == nil {
		return "", "", false
	}
	allocation := al.allocateRouteSession(route, msg)

	return resolveScopeKey(allocation.SessionKey, msg.SessionKey), agent.ID, true
}
