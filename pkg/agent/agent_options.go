package agent

import runtimeevents "github.com/rlds/chagent/pkg/events"

// AgentLoopOption 在构造时配置 AgentLoop。
type AgentLoopOption func(*AgentLoop)

// WithRuntimeEvents 注入用于新观测 API 的运行时事件总线。
//
// 注入的总线视为由外部所有，AgentLoop.Close 不会将其关闭。
// 传入 nil 时保留默认的自有运行时总线。
func WithRuntimeEvents(bus runtimeevents.Bus) AgentLoopOption {
	return func(al *AgentLoop) {
		if bus == nil {
			return
		}
		al.runtimeEvents = bus
		al.ownsRuntimeEvents = false
	}
}
