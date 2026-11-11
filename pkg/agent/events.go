package agent

import (
	"time"

	runtimeevents "github.com/rlds/chagent/pkg/events"
)

// HookMeta 包含 agent 钩子请求与轮次处理过程中发出的
// 运行时事件共用的关联字段。
type HookMeta struct {
	AgentID      string
	TurnID       string
	ParentTurnID string
	SessionKey   string
	Iteration    int
	TracePath    string
	Source       string
	turnContext  *TurnContext
}

// EventKind 是建立在运行时事件总线之上的旧版 agent 内事件类型别名，
// 为测试和兼容垫片保留。
type EventKind = runtimeevents.Kind

const (
	EventKindTurnStart              EventKind = runtimeevents.KindAgentTurnStart
	EventKindTurnEnd                EventKind = runtimeevents.KindAgentTurnEnd
	EventKindLLMRequest             EventKind = runtimeevents.KindAgentLLMRequest
	EventKindLLMDelta               EventKind = runtimeevents.KindAgentLLMDelta
	EventKindLLMResponse            EventKind = runtimeevents.KindAgentLLMResponse
	EventKindLLMRetry               EventKind = runtimeevents.KindAgentLLMRetry
	EventKindContextCompress        EventKind = runtimeevents.KindAgentContextCompress
	EventKindSessionSummarize       EventKind = runtimeevents.KindAgentSessionSummarize
	EventKindToolExecStart          EventKind = runtimeevents.KindAgentToolExecStart
	EventKindToolExecEnd            EventKind = runtimeevents.KindAgentToolExecEnd
	EventKindToolExecSkipped        EventKind = runtimeevents.KindAgentToolExecSkipped
	EventKindSteeringInjected       EventKind = runtimeevents.KindAgentSteeringInjected
	EventKindFollowUpQueued         EventKind = runtimeevents.KindAgentFollowUpQueued
	EventKindInterruptReceived      EventKind = runtimeevents.KindAgentInterruptReceived
	EventKindSubTurnSpawn           EventKind = runtimeevents.KindAgentSubTurnSpawn
	EventKindSubTurnEnd             EventKind = runtimeevents.KindAgentSubTurnEnd
	EventKindSubTurnResultDelivered EventKind = runtimeevents.KindAgentSubTurnResultDelivered
	EventKindSubTurnOrphan          EventKind = runtimeevents.KindAgentSubTurnOrphan
	EventKindError                  EventKind = runtimeevents.KindAgentError
)

// EventMeta 是钩子元数据的旧版名称。
type EventMeta = HookMeta

// Event 是由 SubscribeEvents 和少量测试暴露的旧版 agent 事件信封。
// 运行时代码在内部发布 pkg/events.Event。
type Event struct {
	Kind    EventKind
	Time    time.Time
	Meta    EventMeta
	Context *TurnContext
	Payload any
}
