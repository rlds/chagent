// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"fmt"

	runtimeevents "github.com/rlds/chagent/pkg/events"
)

func (al *AgentLoop) newTurnEventScope(agentID, sessionKey string, turnCtx *TurnContext) turnEventScope {
	seq := al.turnSeq.Add(1)
	return turnEventScope{
		agentID:    agentID,
		sessionKey: sessionKey,
		turnID:     fmt.Sprintf("%s-turn-%d", agentID, seq),
		context:    cloneTurnContext(turnCtx),
	}
}

func (ts turnEventScope) meta(iteration int, source, tracePath string) HookMeta {
	return HookMeta{
		AgentID:     ts.agentID,
		TurnID:      ts.turnID,
		SessionKey:  ts.sessionKey,
		Iteration:   iteration,
		Source:      source,
		TracePath:   tracePath,
		turnContext: cloneTurnContext(ts.context),
	}
}

func (al *AgentLoop) emitEvent(kind runtimeevents.Kind, meta HookMeta, payload any) {
	clonedMeta := cloneHookMeta(meta)
	eventCtx := cloneTurnContext(clonedMeta.turnContext)
	evt := runtimeevents.Event{
		Kind:        kind,
		Source:      runtimeevents.Source{Component: "agent", Name: clonedMeta.AgentID},
		Scope:       runtimeScopeFromHookMeta(clonedMeta, eventCtx),
		Correlation: runtimeCorrelationFromHookMeta(clonedMeta),
		Severity:    runtimeSeverityForAgentEvent(kind, payload),
		Payload:     payload,
		Attrs:       runtimeAttrsFromHookMeta(clonedMeta),
	}

	if al == nil {
		return
	}

	al.publishRuntimeEvent(evt)
}

// MountHook 在 agent loop 上注册一个进程内 hook。
func (al *AgentLoop) MountHook(reg HookRegistration) error {
	if al == nil || al.hooks == nil {
		return fmt.Errorf("hook manager is not initialized")
	}
	return al.hooks.Mount(reg)
}

// UnmountHook 移除先前注册的进程内 hook。
func (al *AgentLoop) UnmountHook(name string) {
	if al == nil || al.hooks == nil {
		return
	}
	al.hooks.Unmount(name)
}

// RuntimeEvents 返回根运行时事件通道。
func (al *AgentLoop) RuntimeEvents() runtimeevents.EventChannel {
	if al == nil || al.runtimeEvents == nil {
		return nil
	}
	return al.runtimeEvents.Channel()
}

// RuntimeEventStats 返回运行时事件总线的计数器。
func (al *AgentLoop) RuntimeEventStats() runtimeevents.Stats {
	if al == nil || al.runtimeEvents == nil {
		return runtimeevents.Stats{Closed: true}
	}
	return al.runtimeEvents.Stats()
}

// RuntimeEventBus 返回 agent loop 使用的运行时事件总线。
func (al *AgentLoop) RuntimeEventBus() runtimeevents.Bus {
	if al == nil {
		return nil
	}
	return al.runtimeEvents
}
