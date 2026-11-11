// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
	"github.com/rlds/chagent/pkg/constants"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/routing"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/utils"
)

func (al *AgentLoop) buildContinuationTarget(msg bus.InboundMessage) (*continuationTarget, error) {
	if msg.Channel == "system" {
		return nil, nil
	}

	route, _, err := al.resolveMessageRoute(msg)
	if err != nil {
		return nil, err
	}
	allocation := al.allocateRouteSession(route, msg)

	return &continuationTarget{
		SessionKey: resolveScopeKey(allocation.SessionKey, msg.SessionKey),
		Channel:    msg.Channel,
		ChatID:     msg.ChatID,
	}, nil
}

func (al *AgentLoop) ProcessDirect(
	ctx context.Context,
	content, sessionKey string,
) (string, error) {
	return al.ProcessDirectWithChannel(ctx, content, sessionKey, "cli", "direct")
}

func (al *AgentLoop) ProcessDirectWithChannel(
	ctx context.Context,
	content, sessionKey, channel, chatID string,
) (string, error) {
	if err := al.ensureHooksInitialized(ctx); err != nil {
		return "", err
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		return "", err
	}

	msg := bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:  channel,
			ChatID:   chatID,
			ChatType: "direct",
			SenderID: "cron",
		},
		Content:    content,
		SessionKey: sessionKey,
	}

	resp, _, _, err := al.processMessage(ctx, msg)
	return resp, err
}

func (al *AgentLoop) ProcessHeartbeat(
	ctx context.Context,
	content, channel, chatID string,
) (string, error) {
	if err := al.ensureHooksInitialized(ctx); err != nil {
		return "", err
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		return "", err
	}

	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		return "", fmt.Errorf("no default agent for heartbeat")
	}
	// 长时间的心跳工作使用专用的持久会话，使 TaskState 与历史
	// 能跨 tick 保留。简单的 HEARTBEAT_OK 检查保持无历史，
	// 以避免会话膨胀。
	sessionKey := "autonomy:heartbeat"
	noHistory := strings.TrimSpace(content) == "HEARTBEAT_OK"
	dispatch := DispatchRequest{
		SessionKey:  sessionKey,
		UserMessage: content,
	}
	if channel != "" || chatID != "" {
		dispatch.InboundContext = &bus.InboundContext{
			Channel:  channel,
			ChatID:   chatID,
			ChatType: "direct",
			SenderID: "heartbeat",
		}
	}
	resp, _, _, err := al.runAgentLoop(ctx, agent, processOptions{
		Dispatch:             dispatch,
		DefaultResponse:      defaultResponse,
		EnableSummary:        false,
		SendResponse:         false,
		SuppressToolFeedback: true,
		NoHistory:            noHistory,
		TurnSource:           "scheduler",
	})
	return resp, err
}

func (al *AgentLoop) prepareInboundMessageForAgent(
	ctx context.Context,
	msg bus.InboundMessage,
) bus.InboundMessage {
	msg = bus.NormalizeInboundMessage(msg)
	return msg
}

func (al *AgentLoop) processMessage(ctx context.Context, msg bus.InboundMessage) (string, string, *providers.UsageInfo, error) {
	msg = al.prepareInboundMessageForAgent(ctx, msg)

	// 将消息预览写入日志（错误消息显示完整内容）
	var logContent string
	if strings.Contains(msg.Content, "Error:") || strings.Contains(msg.Content, "error") {
		logContent = msg.Content // 错误消息使用完整内容
	} else {
		logContent = utils.Truncate(msg.Content, 80)
	}
	logger.InfoCF(
		"agent",
		fmt.Sprintf("Processing message from %s:%s: %s", msg.Channel, msg.SenderID, logContent),
		map[string]any{
			"channel":     msg.Channel,
			"chat_id":     msg.ChatID,
			"sender_id":   msg.SenderID,
			"session_key": msg.SessionKey,
		},
	)

	// 将系统消息路由到 processSystemMessage
	if msg.Channel == "system" {
		resp, err := al.processSystemMessage(ctx, msg)
		return resp, "", nil, err
	}

	route, agent, routeErr := al.resolveMessageRoute(msg)
	if routeErr != nil {
		return "", "", nil, routeErr
	}

	allocation := al.allocateRouteSession(route, msg)

	// 从路由分配结果解析 session key，同时保留调用方
	// 提供的显式 agent 作用域 key。
	scopeKey := resolveScopeKey(allocation.SessionKey, msg.SessionKey)
	sessionKey := scopeKey

	// T5.4/T5.7：显式用户输入始终抢占排队的自主工作。
	// 延迟的自主轮次绝不能运行在新的用户消息之上；
	// /stop 还会暂停任务，使其之后无法被复活。
	al.cancelPendingAutonomyRound(scopeKey, AutonomyRoundReasonUserStop)
	if cmdName, ok := commands.CommandName(msg.Content); ok && cmdName == "stop" {
		al.pauseTaskStateForStop(agent, scopeKey)
	}

	// 重置本轮的 message 工具状态，避免因上一轮而跳过发布。
	if tool, ok := agent.Tools.Get("message"); ok {
		if resetter, ok := tool.(interface{ ResetSentInRound(sessionKey string) }); ok {
			resetter.ResetSentInRound(sessionKey)
		}
	}

	logger.InfoCF("agent", "Routed message",
		map[string]any{
			"agent_id":           agent.ID,
			"scope_key":          scopeKey,
			"session_key":        sessionKey,
			"matched_by":         route.MatchedBy,
			"route_agent":        route.AgentID,
			"route_channel":      route.Channel,
			"route_main_session": allocation.MainSessionKey,
		})

	opts := processOptions{
		Dispatch: DispatchRequest{
			SessionKey:     sessionKey,
			SessionAliases: buildSessionAliases(sessionKey, append(allocation.SessionAliases, msg.SessionKey)...),
			InboundContext: cloneInboundContext(&msg.Context),
			RouteResult:    cloneResolvedRoute(&route),
			SessionScope:   session.CloneScope(&allocation.Scope),
			UserMessage:    msg.Content,
			Media:          append([]string(nil), msg.Media...),
		},
		SenderID:                msg.SenderID,
		SenderDisplayName:       msg.Sender.DisplayName,
		DefaultResponse:         defaultResponse,
		EnableSummary:           true,
		SendResponse:            false,
		AllowInterimPicoPublish: true,
	}
	var err error
	opts, err = resolveTurnProfileOptions(al.GetConfig(), opts)
	if err != nil {
		return "", "", nil, err
	}

	// 依赖上下文的命令自行检查各自的 Runtime 字段，并在所需能力为 nil 时
	// 报告 "unavailable"。
	if response, handled := al.handleCommand(ctx, msg, agent, &opts); handled {
		return response, "", nil, nil
	}

	if pending := al.takePendingSkills(opts.Dispatch.SessionKey); len(pending) > 0 {
		opts.ForcedSkills = append(opts.ForcedSkills, pending...)
		logger.InfoCF("agent", "Applying pending skill override",
			map[string]any{
				"session_key": opts.Dispatch.SessionKey,
				"skills":      strings.Join(pending, ","),
			})
	}

	resp, modelName, usage, err := al.runAgentLoop(ctx, agent, opts)
	return resp, modelName, usage, err
}

func (al *AgentLoop) resolveMessageRoute(msg bus.InboundMessage) (routing.ResolvedRoute, *AgentInstance, error) {
	registry := al.GetRegistry()
	inboundCtx := normalizedInboundContext(msg)
	route := registry.ResolveRoute(inboundCtx)

	agent, ok := registry.GetAgent(route.AgentID)
	if !ok {
		agent = registry.GetDefaultAgent()
	}
	if agent == nil {
		return routing.ResolvedRoute{}, nil, fmt.Errorf("没有可用于路由的 agent（agent_id=%s）", route.AgentID)
	}

	return route, agent, nil
}

func (al *AgentLoop) allocateRouteSession(route routing.ResolvedRoute, msg bus.InboundMessage) session.Allocation {
	return session.AllocateRouteSession(session.AllocationInput{
		AgentID:       route.AgentID,
		Context:       normalizedInboundContext(msg),
		SessionPolicy: route.SessionPolicy,
	})
}

func (al *AgentLoop) processSystemMessage(
	ctx context.Context,
	msg bus.InboundMessage,
) (string, error) {
	if msg.Channel != "system" {
		return "", fmt.Errorf(
			"processSystemMessage called with non-system message channel: %s",
			msg.Channel,
		)
	}

	logger.InfoCF("agent", "Processing system message",
		map[string]any{
			"sender_id": msg.SenderID,
			"chat_id":   msg.ChatID,
		})

	// 从 chat_id 解析来源渠道（格式："channel:chat_id"）
	var originChannel, originChatID string
	if idx := strings.Index(msg.ChatID, ":"); idx > 0 {
		originChannel = msg.ChatID[:idx]
		originChatID = msg.ChatID[idx+1:]
	} else {
		originChannel = "cli"
		originChatID = msg.ChatID
	}

	// 从消息内容中提取 subagent 结果
	// 格式："Task 'label' completed.\n\nResult:\n<actual content>"
	content := msg.Content
	if idx := strings.Index(content, "Result:\n"); idx >= 0 {
		content = content[idx+8:] // 仅提取结果部分
	}

	// 跳过内部渠道——仅记录日志，不发送给用户
	if constants.IsInternalChannel(originChannel) {
		logger.InfoCF("agent", "Subagent completed (internal channel)",
			map[string]any{
				"sender_id":   msg.SenderID,
				"content_len": len(content),
				"channel":     originChannel,
			})
		return "", nil
	}

	// 系统消息使用默认 agent
	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		return "", fmt.Errorf("no default agent for system message")
	}

	// 使用来源会话作为上下文
	sessionKey := session.BuildMainSessionKey(agent.ID)
	dispatch := DispatchRequest{
		SessionKey:  sessionKey,
		UserMessage: fmt.Sprintf("[系统：%s] %s", msg.SenderID, msg.Content),
	}
	if originChannel != "" || originChatID != "" {
		dispatch.InboundContext = &bus.InboundContext{
			Channel:  originChannel,
			ChatID:   originChatID,
			ChatType: "direct",
			SenderID: msg.SenderID,
		}
	}

	resp, _, _, err := al.runAgentLoop(ctx, agent, processOptions{
		Dispatch:        dispatch,
		DefaultResponse: "后台任务已完成。",
		EnableSummary:   false,
		SendResponse:    true,
	})
	return resp, err
}
