package agent

import "time"

// TurnEndStatus 描述回合的终态。
type TurnEndStatus string

const (
	// TurnEndStatusCompleted 表示回合正常结束。
	TurnEndStatusCompleted TurnEndStatus = "completed"
	// TurnEndStatusError 表示回合因错误而结束。
	TurnEndStatusError TurnEndStatus = "error"
	// TurnEndStatusAborted 表示回合被强制中止并回滚。
	TurnEndStatusAborted TurnEndStatus = "aborted"
)

// TurnStartPayload 描述回合的开始。
type TurnStartPayload struct {
	UserMessage string
	MediaCount  int
}

const (
	skillContextTriggerInitialBuild        = "initial_build"
	skillContextTriggerContextRetryRebuild = "context_retry_rebuild"
)

type SkillContextSnapshot struct {
	Sequence   int      `json:"sequence"`
	Trigger    string   `json:"trigger"`
	SkillNames []string `json:"skill_names,omitempty"`
}

type ToolExecutionRecord struct {
	Name         string   `json:"name"`
	Success      bool     `json:"success"`
	ErrorSummary string   `json:"error_summary,omitempty"`
	SkillNames   []string `json:"skill_names,omitempty"`
}

// TurnEndPayload 描述回合的完成。
type TurnEndPayload struct {
	Status                TurnEndStatus
	Workspace             string
	Iterations            int
	Duration              time.Duration
	FinalContentLen       int
	UserMessage           string
	FinalContent          string
	ActiveSkills          []string
	AttemptedSkills       []string
	FinalSuccessfulPath   []string
	SkillContextSnapshots []SkillContextSnapshot
	ToolKinds             []string
	ToolExecutions        []ToolExecutionRecord
}

// LLMRequestPayload 描述一次出站 LLM 请求。
type LLMRequestPayload struct {
	Model         string
	MessagesCount int
	ToolsCount    int
	MaxTokens     int
	Temperature   float64
}

// LLMResponsePayload 描述一次入站 LLM 响应。
type LLMResponsePayload struct {
	ContentLen   int
	ToolCalls    int
	HasReasoning bool
}

// LLMDeltaPayload 描述一次流式 LLM 增量。
type LLMDeltaPayload struct {
	ContentDeltaLen   int
	ReasoningDeltaLen int
}

// LLMRetryPayload 描述一次 LLM 请求重试。
type LLMRetryPayload struct {
	Attempt    int
	MaxRetries int
	Reason     string
	Error      string
	Backoff    time.Duration
}

// ContextCompressReason 标识紧急压缩运行的原因。
type ContextCompressReason string

const (
	// ContextCompressReasonProactive 表示首次 LLM 调用前的压缩。
	ContextCompressReasonProactive ContextCompressReason = "proactive_budget"
	// ContextCompressReasonRetry 表示上下文错误重试处理期间的压缩。
	ContextCompressReasonRetry ContextCompressReason = "llm_retry"
	// ContextCompressReasonSummarize 表示回合后的异步摘要。
	ContextCompressReasonSummarize ContextCompressReason = "summarize"
)

// ContextCompressPayload 描述一次强制历史压缩。
type ContextCompressPayload struct {
	Reason            ContextCompressReason
	DroppedMessages   int
	RemainingMessages int
}

// SessionSummarizePayload 描述一次已完成的异步会话摘要。
type SessionSummarizePayload struct {
	SummarizedMessages int
	KeptMessages       int
	SummaryLen         int
	OmittedOversized   bool
}

// ToolExecStartPayload 描述一次工具执行请求。
type ToolExecStartPayload struct {
	Tool      string
	Arguments map[string]any
}

// ToolExecEndPayload 描述一次工具执行的结果。
type ToolExecEndPayload struct {
	Tool       string
	Duration   time.Duration
	ForLLMLen  int
	ForUserLen int
	IsError    bool
	Async      bool
}

// ToolExecSkippedPayload 描述一次被跳过的工具调用。
type ToolExecSkippedPayload struct {
	Tool   string
	Reason string
}

// SteeringInjectedPayload 描述在下一次 LLM 调用前追加的 steering 消息。
type SteeringInjectedPayload struct {
	Count           int
	TotalContentLen int
}

// AutonomyRoundPhase 标识单个自主轮次的生命周期阶段。
type AutonomyRoundPhase string

const (
	// AutonomyRoundPhaseScheduled 表示驱动排队了下一轮。
	AutonomyRoundPhaseScheduled AutonomyRoundPhase = "scheduled"
	// AutonomyRoundPhaseStarted 表示该轮次的回合已经开始。
	AutonomyRoundPhaseStarted AutonomyRoundPhase = "started"
	// AutonomyRoundPhaseEnded 表示轮次结束，驱动已决定是否继续。
	AutonomyRoundPhaseEnded AutonomyRoundPhase = "ended"
)

// AutonomyRoundReason 解释轮次结束或驱动停止的原因。
type AutonomyRoundReason string

const (
	// AutonomyRoundReasonCompleted 表示任务到达 complete 阶段。
	AutonomyRoundReasonCompleted AutonomyRoundReason = "completed"
	// AutonomyRoundReasonBlocked 表示任务到达 blocked 阶段。
	AutonomyRoundReasonBlocked AutonomyRoundReason = "blocked"
	// AutonomyRoundReasonRoundLimit 表示已达到 MaxRounds。
	AutonomyRoundReasonRoundLimit AutonomyRoundReason = "round-limit"
	// AutonomyRoundReasonNoProgress 表示连续轮次没有进展。
	AutonomyRoundReasonNoProgress AutonomyRoundReason = "no-progress"
	// AutonomyRoundReasonUserStop 表示用户停止/暂停或中断了任务，
	// 因此自动续跑被取消。
	AutonomyRoundReasonUserStop AutonomyRoundReason = "user-stop"
	// AutonomyRoundReasonProviderError 表示 LLM 调用在重试后失败，
	// 并记录了恢复检查点而不是完成任务。
	AutonomyRoundReasonProviderError AutonomyRoundReason = "provider-error"
	// AutonomyRoundReasonEmptyRound 表示轮次结束但未调度新轮次，
	// 因为任务状态已不再活跃。
	AutonomyRoundReasonEmptyRound AutonomyRoundReason = "empty-round"
	// AutonomyRoundReasonResume 表示轮次由 resume-on-start 调度。
	AutonomyRoundReasonResume AutonomyRoundReason = "resume"
)

// AutonomyRoundPayload 描述一次 autonomy 驱动决策。
type AutonomyRoundPayload struct {
	Round     int
	MaxRounds int
	Phase     AutonomyRoundPhase
	Reason    AutonomyRoundReason
	Objective string
	Source    string
}

// FollowUpQueuedPayload 描述排队回到入站总线的异步跟进消息。
type FollowUpQueuedPayload struct {
	SourceTool string
	ContentLen int
}

type InterruptKind string

const (
	InterruptKindSteering InterruptKind = "steering"
	InterruptKindGraceful InterruptKind = "graceful"
	InterruptKindHard     InterruptKind = "hard_abort"
)

// InterruptReceivedPayload 描述已接受的回合控制输入。
type InterruptReceivedPayload struct {
	Kind       InterruptKind
	Role       string
	ContentLen int
	QueueDepth int
	HintLen    int
}

// SubTurnSpawnPayload 描述子回合的创建。
type SubTurnSpawnPayload struct {
	AgentID      string
	Label        string
	ParentTurnID string
}

// SubTurnEndPayload 描述子回合的完成。
type SubTurnEndPayload struct {
	AgentID string
	Status  string
}

// SubTurnResultDeliveredPayload 描述子回合结果的投递。
type SubTurnResultDeliveredPayload struct {
	TargetChannel string
	TargetChatID  string
	ContentLen    int
}

// SubTurnOrphanPayload 描述无法投递的子回合结果。
type SubTurnOrphanPayload struct {
	ParentTurnID string
	ChildTurnID  string
	Reason       string
}

// ErrorPayload 描述 agent loop 内部的一次执行错误。
type ErrorPayload struct {
	Stage   string
	Message string
}
