package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
)

func (al *AgentLoop) tryHandleStopCommand(
	ctx context.Context,
	msg bus.InboundMessage,
	sessionKey string,
) bool {
	cmdName, ok := commands.CommandName(msg.Content)
	if !ok || cmdName != "stop" {
		return false
	}

	result, err := al.stopActiveTurnForSession(sessionKey)

	// 本函数仅在 loaded=true（已有另一个 turn 认领了该会话）时被调用。如果
	// stopActiveTurnForSession 找到了一个待处理的 placeholder 却未能停止它，
	// 说明该 placeholder 属于另一条消息尚未启动的 worker —— 此时布置一个
	// 待处理停止标记，使该 worker 在运行前的检查中直接退出。
	if err == nil && !result.Stopped {
		if ts := al.getActiveTurnState(sessionKey); ts != nil {
			snap := ts.snapshot()
			if strings.HasPrefix(snap.TurnID, pendingTurnPrefix) {
				al.markPendingStop(sessionKey)
				result.Stopped = true
			}
		}
	}

	reply := commands.FormatStopReply(result)
	if err != nil {
		reply = "停止任务失败：" + err.Error()
	}

	if al.channelManager != nil {
		al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
	}
	al.resetMessageToolRound(sessionKey)
	al.PublishResponseIfNeeded(ctx, msg.Channel, msg.ChatID, sessionKey, reply, "", nil)
	return true
}

func (al *AgentLoop) stopActiveTurnForSession(sessionKey string) (commands.StopResult, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return commands.StopResult{}, fmt.Errorf("session key 不能为空")
	}

	result := commands.StopResult{}
	cleared := al.clearSteeringMessagesForScope(sessionKey)
	al.clearPendingSkills(sessionKey)

	ts := al.getActiveTurnState(sessionKey)
	if ts == nil {
		result.Stopped = cleared > 0
		return result, nil
	}

	snap := ts.snapshot()
	result.TaskName = snap.UserMessage

	if strings.HasPrefix(snap.TurnID, pendingTurnPrefix) {
		// 待处理 placeholder 意味着该会话要么空闲（即 /stop 命令自身留下的
		// placeholder），要么另一条消息已排队但尚未启动。两种情况下都不在此处
		// 布置待处理停止标记；调用方（tryHandleStopCommand）知道 loaded=true，
		// 会显式处理“另一条消息已排队”的情形。
		return result, nil
	}

	al.pauseTaskStateForStop(ts.agent, sessionKey)

	if err := al.HardAbort(sessionKey); err != nil {
		if al.getActiveTurnState(sessionKey) == nil {
			result.Stopped = cleared > 0
			return result, nil
		}
		return commands.StopResult{}, err
	}

	result.Stopped = true
	return result, nil
}

func (al *AgentLoop) markPendingStop(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	al.pendingStops.Store(sessionKey, struct{}{})
}

func (al *AgentLoop) takePendingStop(sessionKey string) bool {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return false
	}
	_, ok := al.pendingStops.LoadAndDelete(sessionKey)
	return ok
}

func (al *AgentLoop) resetMessageToolRound(sessionKey string) {
	if strings.TrimSpace(sessionKey) == "" {
		return
	}
	if registry := al.GetRegistry(); registry != nil {
		if agent := registry.GetDefaultAgent(); agent != nil {
			if tool, ok := agent.Tools.Get("message"); ok {
				if resetter, ok := tool.(interface{ ResetSentInRound(sessionKey string) }); ok {
					resetter.ResetSentInRound(sessionKey)
				}
			}
		}
	}
}
