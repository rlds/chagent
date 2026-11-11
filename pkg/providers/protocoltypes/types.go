package protocoltypes

import "time"

type ToolCall struct {
	ID               string         `json:"id"`
	Type             string         `json:"type,omitempty"`
	Function         *FunctionCall  `json:"function,omitempty"`
	Name             string         `json:"-"`
	Arguments        map[string]any `json:"-"`
	ThoughtSignature string         `json:"-"` // 仅内部使用
	ExtraContent     *ExtraContent  `json:"extra_content,omitempty"`
}

type ExtraContent struct {
	Google                  *GoogleExtra `json:"google,omitempty"`
	ToolFeedbackExplanation string       `json:"tool_feedback_explanation,omitempty"`
}

type GoogleExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type FunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type LLMResponse struct {
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	FinishReason     string            `json:"finish_reason"`
	Usage            *UsageInfo        `json:"usage,omitempty"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
	// Model 是 API 响应中返回的模型名（例如 "gpt-4o-2024-08-06"）。
	// 空字符串表示 provider 未返回该字段。
	Model string `json:"model,omitempty"`
}

type StreamChunk struct {
	Content          string
	ReasoningContent string
	// Model 是本流式分片中报告的模型名（如有）。
	// 当 provider 不在每个分片中附带 model 字段时为空。
	Model string
}

type ReasoningDetail struct {
	Format string `json:"format"`
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Text   string `json:"text"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// 缓存命中 / 前缀缓存 token 数（OpenAI 兼容格式）。
	// 由支持该字段的 provider 填充（DeepSeek、GLM、OpenAI 等）。
	// 0 表示未发生缓存，或该 provider 未报告此值。
	CachedTokens int `json:"cached_tokens,omitempty"`

	// Anthropic 风格的缓存字段。
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`

	// 推理 / 思考 token 数（来自 completion_tokens_details.reasoning_tokens）。
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// CacheControl 将某个内容块标记为供 LLM 侧前缀缓存使用。
// 目前仅支持 "ephemeral"（由 Anthropic 使用）。
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// ContentBlock 表示系统消息中的一个结构化片段。
// 能理解 SystemParts 的适配器可以使用这些块来设置
// 逐块的缓存控制（例如 Anthropic 的 cache_control: ephemeral）。
type ContentBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// Prompt 元数据属于 agent 运行时内部信息。它记录是哪个
	// 结构化 prompt 片段生成了该块，且不改变 provider 的 JSON。
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type Attachment struct {
	Type        string `json:"type,omitempty"`
	Ref         string `json:"ref,omitempty"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ModelName        string         `json:"model_name,omitempty"`
	CreatedAt        *time.Time     `json:"created_at,omitempty"`
	Media            []string       `json:"media,omitempty"`
	Attachments      []Attachment   `json:"attachments,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	SystemParts      []ContentBlock `json:"system_parts,omitempty"` // 供具备缓存感知的适配器使用的结构化系统块
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`

	// Context 将消息标记为运行时注入的上下文（工作区
	// 指令、运行时快照、任务状态等），而非普通的
	// 对话历史。它会随会话一起持久化，但绝不能被
	// 发送给 provider；provider 序列化器会显式挑选字段，
	// 因此会忽略该字段。
	Context *ContextMeta `json:"context,omitempty"`

	// Prompt 元数据属于 agent 运行时内部信息。它记录某条消息
	// 或系统片段的来源，且不改变 provider/会话的 JSON。
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

// Context 消息种类。这些是持久化在会话历史中的稳定标识符；
// 切勿在调用点内联原始字符串。
const (
	ContextKindWorkspaceInstructions = "workspace-instructions"
	ContextKindRuntimeContext        = "runtime-context"
	ContextKindTaskState             = "task-state"
	ContextKindTaskRound             = "task-round"
	ContextKindSkillCatalog          = "skill-catalog"
	ContextKindLoopGuard             = "loop-guard"
	ContextKindContextPressure       = "context-pressure"
)

// Context 形态描述上下文消息的渲染方式。
const (
	ContextFormReminder = "reminder"
	ContextFormMessage  = "message"
)

// 基线/增量对账所使用的上下文变更动作。
const (
	ContextActionSet     = "set"
	ContextActionReplace = "replace"
	ContextActionRemove  = "remove"
)

// ContextMeta 是附加在注入上下文消息上的类型化来源元数据。
// 它让 agent 能区分插件注入的上下文与真实用户回合，
// 也让恢复/压缩流程能对已注入的内容进行对账。
type ContextMeta struct {
	Kind             string           `json:"kind"`
	Form             string           `json:"form,omitempty"`
	Baseline         bool             `json:"baseline,omitempty"`
	BaselineIdentity string           `json:"baseline_identity,omitempty"`
	Digest           string           `json:"digest,omitempty"`
	Changes          []ContextChange  `json:"changes,omitempty"`
	Sections         []ContextSection `json:"sections,omitempty"`
}

// ContextChange 记录一次针对某个工作区来源的基线/增量操作。
type ContextChange struct {
	Action string `json:"action"` // set | replace | remove
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}

// ContextSection 是一个具名且已渲染的上下文小节。
type ContextSection struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// IsContextMessage 报告该消息是否由运行时的上下文机制注入，
// 而非由用户或模型产生。
func (m Message) IsContextMessage() bool {
	return m.Context != nil && m.Context.Kind != ""
}

// IsWorkspaceInstructions 报告该消息是否携带工作区指令上下文
// （项目/用户指令文件）。
func (m Message) IsWorkspaceInstructions() bool {
	return m.IsContextMessage() && m.Context.Kind == ContextKindWorkspaceInstructions
}

// IsRuntimeContext 报告该消息是否携带运行时快照。
func (m Message) IsRuntimeContext() bool {
	return m.IsContextMessage() && m.Context.Kind == ContextKindRuntimeContext
}

// IsTaskState 报告该消息是否携带已渲染的 TaskState 上下文。
func (m Message) IsTaskState() bool {
	return m.IsContextMessage() && m.Context.Kind == ContextKindTaskState
}

// IsLoopGuard 报告消息是否携带循环卫生提醒（例如重复工具调用守卫）。
// loop-guard 消息是提示性的合成用户消息，绝不是真实用户回合。
func (m Message) IsLoopGuard() bool {
	return m.IsContextMessage() && m.Context.Kind == ContextKindLoopGuard
}

// IsContextPressure 报告消息是否携带提示性的接近上下文上限通知。
// 与 loop-guard 消息一样，它是合成的、非回合起点的上下文消息。
func (m Message) IsContextPressure() bool {
	return m.IsContextMessage() && m.Context.Kind == ContextKindContextPressure
}

type ToolDefinition struct {
	Type     string                 `json:"type"`
	Function ToolFunctionDefinition `json:"function"`

	// Prompt 元数据属于 agent 运行时内部信息。工具定义是
	// 模型可见的能力提示词，即使 provider 会把它们放在系统消息之外发送。
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type ToolFunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
