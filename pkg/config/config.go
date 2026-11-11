package config

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/logger"
	providercommon "github.com/rlds/chagent/pkg/providers/common"
)

// rrCounter 是用于在多个模型之间进行轮询负载均衡的全局计数器。
var rrCounter atomic.Uint64

// CurrentVersion 是最新的配置模式版本。
const CurrentVersion = 2

func init() {
	initChannel()
}

// Config 是当前支持版本控制的配置结构体。
type Config struct {
	// 用于迁移的配置模式版本。
	Version   int             `json:"version"             yaml:"-"`
	Isolation IsolationConfig `json:"isolation,omitempty" yaml:"-"`
	Agents    AgentsConfig    `json:"agents"              yaml:"-"`
	Session   SessionConfig   `json:"session,omitempty"   yaml:"-"`
	Channels  ChannelsConfig  `json:"channels"              yaml:"channels"`
	ModelList SecureModelList `json:"model_list"          yaml:"model_list"` // 新的以模型为中心的提供商配置
	Gateway   GatewayConfig   `json:"gateway"             yaml:"-"`
	Events    EventsConfig    `json:"events,omitempty"    yaml:"-"`
	Hooks     HooksConfig     `json:"hooks,omitempty"     yaml:"-"`
	Tools     ToolsConfig     `json:"tools"               yaml:",inline"`
	Heartbeat HeartbeatConfig `json:"heartbeat"           yaml:"-"`
	// BuildInfo 包含构建时的版本信息。
	BuildInfo BuildInfo       `json:"build_info,omitempty" yaml:"-"`
	AgentInfo AgentInfoConfig `json:"agentinfo,omitempty" yaml:"-"`
	// 敏感值与已编译正则表达式的缓存（只计算一次）
	sensitiveCache *SensitiveDataCache
}

type AgentInfoConfig struct {
	MOLEOwner       string `json:"MOLE_OWNER,omitempty"`
	MOLESourceAgent string `json:"MOLE_SOURCE_AGENT,omitempty"`
	MOLEUserID      string `json:"MOLE_USER_ID,omitempty"`
}

// IsolationConfig 控制 Chagent 启动的命令的子进程隔离。
// 它由 isolation 包负责应用，而不是通过沙箱化主进程来实现。
type IsolationConfig struct {
	Enabled     bool         `json:"enabled,omitempty"`
	ExposePaths []ExposePath `json:"expose_paths,omitempty"`
}

// ExposePath 描述在隔离的子进程环境中仍需保持可见的主机路径。
// 目前仅在 Linux 上实现。
type ExposePath struct {
	Source string `json:"source"`
	Target string `json:"target,omitempty"`
	Mode   string `json:"mode"`
}

// FilterSensitiveData 在发送给 LLM 之前过滤内容中的敏感值，
// 从而避免 LLM 看到自身的凭据。
// 使用 strings.Replacer 以获得 O(n+m) 的性能（每个 SecurityConfig 只计算一次）。
// 出于性能考虑，较短的内容（低于 FilterMinLength）将原样返回。
func (c *Config) FilterSensitiveData(content string) string {
	if c == nil {
		return content
	}
	// 检查是否启用了过滤（默认：启用）
	if !c.Tools.IsFilterSensitiveDataEnabled() {
		return content
	}
	// 快速路径：对较短内容跳过过滤
	if len(content) < c.Tools.GetFilterMinLength() {
		return content
	}
	return c.SensitiveDataReplacer().Replace(content)
}

type HooksConfig struct {
	Enabled   bool                         `json:"enabled"`
	Defaults  HookDefaultsConfig           `json:"defaults,omitempty"`
	Builtins  map[string]BuiltinHookConfig `json:"builtins,omitempty"`
	Processes map[string]ProcessHookConfig `json:"processes,omitempty"`
}

type HookDefaultsConfig struct {
	ObserverTimeoutMS    int `json:"observer_timeout_ms,omitempty"`
	InterceptorTimeoutMS int `json:"interceptor_timeout_ms,omitempty"`
	ApprovalTimeoutMS    int `json:"approval_timeout_ms,omitempty"`
}

type BuiltinHookConfig struct {
	Enabled  bool            `json:"enabled"`
	Priority int             `json:"priority,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}

type ProcessHookConfig struct {
	Enabled   bool              `json:"enabled"`
	Priority  int               `json:"priority,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Observe   []string          `json:"observe,omitempty"`
	Intercept []string          `json:"intercept,omitempty"`
}

// BuildInfo 包含构建时的版本信息。
type BuildInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// MarshalJSON 为 Config 实现自定义 JSON 序列化，
// 以在 providers 段和 session 段为空时省略它们。
func (c *Config) MarshalJSON() ([]byte, error) {
	type Alias Config
	aux := &struct {
		Session *SessionConfig `json:"session,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(c),
	}

	if len(c.Session.Dimensions) > 0 || len(c.Session.IdentityLinks) > 0 || c.Session.DmScope != "" {
		sessionCfg := c.Session
		aux.Session = &sessionCfg
	}

	return json.Marshal(aux)
}

type AgentsConfig struct {
	Defaults AgentDefaults   `json:"defaults"`
	List     []AgentConfig   `json:"list,omitempty"`
	Dispatch *DispatchConfig `json:"dispatch,omitempty"`
}

// AgentModelConfig 同时支持字符串形式和结构化模型配置。
// 字符串形式："gpt-4"（仅主模型，无回退）。
// 对象形式：{"primary": "gpt-4", "fallbacks": ["claude-haiku"]}
type AgentModelConfig struct {
	Primary   string   `json:"primary,omitempty"`
	Fallbacks []string `json:"fallbacks,omitempty"`
}

func (m *AgentModelConfig) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m.Primary = s
		m.Fallbacks = nil
		return nil
	}
	type raw struct {
		Primary   string   `json:"primary"`
		Fallbacks []string `json:"fallbacks"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	m.Primary = r.Primary
	m.Fallbacks = r.Fallbacks
	return nil
}

func (m AgentModelConfig) MarshalJSON() ([]byte, error) {
	if len(m.Fallbacks) == 0 && m.Primary != "" {
		return json.Marshal(m.Primary)
	}
	type raw struct {
		Primary   string   `json:"primary,omitempty"`
		Fallbacks []string `json:"fallbacks,omitempty"`
	}
	return json.Marshal(raw{Primary: m.Primary, Fallbacks: m.Fallbacks})
}

type AgentConfig struct {
	ID        string            `json:"id"`
	Default   bool              `json:"default,omitempty"`
	Name      string            `json:"name,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	Model     *AgentModelConfig `json:"model,omitempty"`
	Skills    []string          `json:"skills,omitempty"`
	Subagents *SubagentsConfig  `json:"subagents,omitempty"`
}

type SubagentsConfig struct {
	AllowAgents []string          `json:"allow_agents,omitempty"`
	Model       *AgentModelConfig `json:"model,omitempty"`
}

type DispatchConfig struct {
	Rules []DispatchRule `json:"rules,omitempty"`
}

type DispatchRule struct {
	Name              string           `json:"name,omitempty"`
	Agent             string           `json:"agent"`
	When              DispatchSelector `json:"when"`
	SessionDimensions []string         `json:"session_dimensions,omitempty"`
}

type DispatchSelector struct {
	Channel   string `json:"channel,omitempty"`
	Account   string `json:"account,omitempty"`
	Space     string `json:"space,omitempty"`
	Chat      string `json:"chat,omitempty"`
	Topic     string `json:"topic,omitempty"`
	Sender    string `json:"sender,omitempty"`
	Mentioned *bool  `json:"mentioned,omitempty"`
}

type SessionConfig struct {
	Dimensions    []string            `json:"dimensions,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty"`
	DmScope       string              `json:"dm_scope,omitempty"`
}

// ApplyDmScope 把面向用户的 dm_scope 值转换为路由层所使用的内部
// dimensions 数组。当 DmScope 为空或 Dimensions 已设置时不执行任何操作
// （显式设置的 Dimensions 优先于派生值）。
func (s *SessionConfig) ApplyDmScope() {
	if s.DmScope == "" || len(s.Dimensions) > 0 {
		return
	}
	switch s.DmScope {
	case "per-channel-peer":
		s.Dimensions = []string{"chat", "sender"}
	case "per-channel":
		s.Dimensions = []string{"chat"}
	case "per-peer":
		s.Dimensions = []string{"sender"}
	case "global":
		s.Dimensions = nil
	}
}

// DeriveDmScope 在 DmScope 为空时根据 Dimensions 设置 DmScope。
// 这用于处理只显式设置了 Dimensions、却没有对应 DmScope 值的
// 遗留或新建配置，确保 API 响应始终包含与实际运行时维度一致的 dm_scope。
func (s *SessionConfig) DeriveDmScope() {
	if s.DmScope != "" || len(s.Dimensions) == 0 {
		return
	}
	switch {
	case slices.Equal(s.Dimensions, []string{"chat", "sender"}):
		s.DmScope = "per-channel-peer"
	case slices.Equal(s.Dimensions, []string{"chat"}):
		s.DmScope = "per-channel"
	case slices.Equal(s.Dimensions, []string{"sender"}):
		s.DmScope = "per-peer"
	}
	// Dimensions 不匹配任何已知的作用域映射（自定义数组）也没有问题——
	// DmScope 保持为空，由 UI 自行处理。
}

// RoutingConfig 控制智能模型路由功能。
// 启用后，每条传入消息都会依据结构化特征（消息长度、代码块、工具调用历史、
// 对话深度、附件）进行评分。评分低于 Threshold 的消息会发送给 LightModel，
// 其余消息使用 agent 的主模型。这样可以在不需要任何关键词匹配的情况下
// 降低简单任务的成本与延迟——全部评分过程与语言无关。
type RoutingConfig struct {
	Enabled    bool    `json:"enabled"`
	LightModel string  `json:"light_model"` // 用于简单任务的 model_list 中的 model_name
	Threshold  float64 `json:"threshold"`   // 复杂度评分，范围 [0,1]；评分 >= threshold 时使用主模型
}

// SubTurnConfig 配置 SubTurn 执行系统。
type SubTurnConfig struct {
	MaxDepth              int `json:"max_depth"`
	MaxConcurrent         int `json:"max_concurrent"`
	DefaultTimeoutMinutes int `json:"default_timeout_minutes"`
	DefaultTokenBudget    int `json:"default_token_budget"`
	ConcurrencyTimeoutSec int `json:"concurrency_timeout_sec"`
}

type ToolFeedbackConfig struct {
	Enabled          bool `json:"enabled"`
	MaxArgsLength    int  `json:"max_args_length"`
	SeparateMessages bool `json:"separate_messages"`
}

type AgentDefaults struct {
	Workspace                 string             `json:"workspace"`
	RestrictToWorkspace       bool               `json:"restrict_to_workspace"`
	AllowReadOutsideWorkspace bool               `json:"allow_read_outside_workspace"`
	Provider                  string             `json:"provider"`
	ModelName                 string             `json:"model_name"`
	ModelFallbacks            []string           `json:"model_fallbacks,omitempty"`
	ImageModel                string             `json:"image_model,omitempty"`
	ImageModelFallbacks       []string           `json:"image_model_fallbacks,omitempty"`
	MaxTokens                 int                `json:"max_tokens"`
	ContextWindow             int                `json:"context_window,omitempty"`
	Temperature               *float64           `json:"temperature,omitempty"`
	MaxToolIterations         int                `json:"max_tool_iterations"`
	SummarizeMessageThreshold int                `json:"summarize_message_threshold"`
	SummarizeTokenPercent     int                `json:"summarize_token_percent"`
	MaxMediaSize              int                `json:"max_media_size,omitempty"`
	Routing                   *RoutingConfig     `json:"routing,omitempty"`
	SteeringMode              string             `json:"steering_mode,omitempty"`      // "one-at-a-time"（默认）或 "all"
	MaxParallelTurns          int                `json:"max_parallel_turns,omitempty"` // 最大并发轮次（0 或 1 表示串行）
	SubTurn                   SubTurnConfig      `json:"subturn"`
	ToolFeedback              ToolFeedbackConfig `json:"tool_feedback,omitempty"`
	SplitOnMarker             bool               `json:"split_on_marker"` // 按 <|[SPLIT]|> 标记拆分消息
	ContextManager            string             `json:"context_manager,omitempty"`
	ContextManagerConfig      json.RawMessage    `json:"context_manager_config,omitempty"`
	TurnProfile               TurnProfileConfig  `json:"turn_profile,omitempty"`
	MaxLLMRetries             int                `json:"max_llm_retries,omitempty"`
	LLMRetryBackoffSecs       int                `json:"llm_retry_backoff_secs,omitempty"`

	// 工作区上下文 / 运行时上下文 / 自主性均为可选；缺少这些键时，
	// 会在加载后应用 chess 配置档的默认值。
	WorkspaceContext WorkspaceContextConfig `json:"workspace_context,omitempty"`
	RuntimeContext   RuntimeContextConfig   `json:"runtime_context,omitempty"`
	Autonomy         AutonomyConfig         `json:"autonomy,omitempty"`
}

const DefaultMaxMediaSize = 20 * 1024 * 1024 // 20 MB

func (d *AgentDefaults) GetMaxMediaSize() int {
	if d.MaxMediaSize > 0 {
		return d.MaxMediaSize
	}
	return DefaultMaxMediaSize
}

// GetToolFeedbackMaxArgsLength 返回工具参数预览的最大可见文本长度。
func (d *AgentDefaults) GetToolFeedbackMaxArgsLength() int {
	if d.ToolFeedback.MaxArgsLength > 0 {
		return d.ToolFeedback.MaxArgsLength
	}
	return 300
}

// IsToolFeedbackEnabled 返回是否应当向聊天发送工具反馈消息。
func (d *AgentDefaults) IsToolFeedbackEnabled() bool {
	return d.ToolFeedback.Enabled
}

// IsToolFeedbackSeparateMessagesEnabled 返回是否应把每条工具反馈更新
// 作为独立的聊天消息发送，而不是原地编辑同一条进度消息。
func (d *AgentDefaults) IsToolFeedbackSeparateMessagesEnabled() bool {
	return d.ToolFeedback.SeparateMessages
}

// GetModelName 返回 agent 默认配置中实际生效的模型名称。
// 它优先使用新的 "model_name" 字段，为向后兼容则回退到 "model"。
func (d *AgentDefaults) GetModelName() string {
	return d.ModelName
}

// GroupTriggerConfig 控制机器人在群聊中何时作出响应。
type GroupTriggerConfig struct {
	MentionOnly bool     `json:"mention_only,omitempty"`
	Prefixes    []string `json:"prefixes,omitempty"`
}

// TypingConfig 控制“正在输入”提示的行为（Phase 10）。
type TypingConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// PlaceholderConfig 控制占位消息的行为（Phase 10）。
type PlaceholderConfig struct {
	Enabled bool                `json:"enabled"`
	Text    FlexibleStringSlice `json:"text,omitempty"`
}

// GetRandomText 返回随机的占位文本；未设置任何文本时返回默认值。
func (p *PlaceholderConfig) GetRandomText() string {
	if len(p.Text) == 0 {
		return "Thinking..."
	}
	if len(p.Text) == 1 {
		return p.Text[0]
	}
	idx := rand.Intn(len(p.Text))
	return p.Text[idx]
}

type StreamingConfig struct {
	Enabled         bool `json:"enabled,omitempty"`
	ThrottleSeconds int  `json:"throttle_seconds,omitempty"`
	MinGrowthChars  int  `json:"min_growth_chars,omitempty"`
}

func (c StreamingConfig) IsZero() bool {
	return !c.Enabled && c.ThrottleSeconds == 0 && c.MinGrowthChars == 0
}

func (c StreamingConfig) WithDefaults(throttleSeconds, minGrowthChars int) StreamingConfig {
	if c.Enabled {
		if c.ThrottleSeconds == 0 {
			c.ThrottleSeconds = throttleSeconds
		}
		if c.MinGrowthChars == 0 {
			c.MinGrowthChars = minGrowthChars
		}
	}
	return c
}

type PicoSettings struct {
	Token           SecureString    `json:"token,omitzero"              yaml:"token,omitempty"`
	AllowTokenQuery bool            `json:"allow_token_query,omitempty" yaml:"-"`
	AllowOrigins    []string        `json:"allow_origins,omitempty"     yaml:"-"`
	Streaming       StreamingConfig `json:"streaming,omitzero"          yaml:"-"`
	PingInterval    int             `json:"ping_interval,omitempty"     yaml:"-"`
	ReadTimeout     int             `json:"read_timeout,omitempty"      yaml:"-"`
	WriteTimeout    int             `json:"write_timeout,omitempty"     yaml:"-"`
	MaxConnections  int             `json:"max_connections,omitempty"   yaml:"-"`
}

// SetToken 设置 Pico token，并将其标记为待保存以持久化安全信息
func (c *PicoSettings) SetToken(token string) {
	c.Token = *NewSecureString(token)
}

type PicoClientSettings struct {
	URL          string       `json:"url"                     yaml:"-"`
	Token        SecureString `json:"token,omitzero"          yaml:"token,omitempty"`
	SessionID    string       `json:"session_id,omitempty"    yaml:"-"`
	PingInterval int          `json:"ping_interval,omitempty" yaml:"-"`
	ReadTimeout  int          `json:"read_timeout,omitempty"  yaml:"-"`
}

// TeamsWebhookTarget 表示单个 Teams webhook 目标。

// SlackWebhookTarget 表示单个 Slack Incoming Webhook 目标。

type HeartbeatConfig struct {
	Enabled  bool `json:"enabled"`
	Interval int  `json:"interval"` // 分钟，最小 5
}

type ModelStreamingConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

func (c ModelStreamingConfig) IsZero() bool {
	return !c.Enabled
}

// ModelConfig 表示以模型为中心的提供商配置。
// 它允许仅通过配置就新增提供商（尤其是 OpenAI 兼容的提供商）。
// Model 字段可以是纯模型标识符，也可以是带提供商前缀的标识符，
// 例如 "openai/gpt-5.4" 或 "nvidia/z-ai/glm-5.1"。
// 仅支持两种通用协议族：OpenAI Chat Completions 兼容协议
// （openai 以及 groq、deepseek、modelscope、novita 等命名的 OpenAI 兼容预设）
// 和 Anthropic Messages 兼容协议（anthropic、anthropic-messages、anthropic-bearer）。
type ModelConfig struct {
	// 必填字段
	ModelName string `json:"model_name"` // 面向用户的模型别名
	Provider  string `json:"provider"`   // 用于路由和选择的提供商名称。为空时，提供商解析会从 Model 中推断。
	Model     string `json:"model"`      // 模型标识符，可带提供商前缀。

	// 基于 HTTP 的提供商
	APIBase   string   `json:"api_base,omitempty"`  // API 端点 URL
	Proxy     string   `json:"proxy,omitempty"`     // HTTP 代理 URL
	Fallbacks []string `json:"fallbacks,omitempty"` // 用于故障转移的回退模型名称

	// Workspace 保留用于向后兼容的配置加载；通用协议
	// 提供商不会使用它。
	Workspace string `json:"workspace,omitempty"` // 工作区路径（遗留 CLI 提供商）

	// 可选的优化项
	RPM                 int                  `json:"rpm,omitempty"`              // 每分钟请求数上限
	MaxTokensField      string               `json:"max_tokens_field,omitempty"` // 最大 token 对应的字段名（例如 "max_completion_tokens"）
	RequestTimeout      int                  `json:"request_timeout,omitempty"`
	ThinkingLevel       string               `json:"thinking_level,omitempty"`        // 扩展思考：off|low|medium|high|xhigh|adaptive
	ToolSchemaTransform string               `json:"tool_schema_transform,omitempty"` // 可选的工具 schema 兼容性转换（例如 "simple"）
	Streaming           ModelStreamingConfig `json:"streaming,omitzero"`              // 是否为此模型条目启用提供商流式输出
	ExtraBody           map[string]any       `json:"extra_body,omitempty"`            // 要注入请求体的额外字段
	CustomHeaders       map[string]string    `json:"custom_headers,omitempty"`        // 要注入每个 HTTP 请求的额外请求头

	APIKeys SecureStrings `json:"api_keys,omitzero" yaml:"api_keys,omitempty"` // API 认证密钥（多个密钥用于故障转移）

	// Enabled 表示此模型条目是否启用。在已有配置中省略该字段时，
	// 会在加载过程中推断：带 API 密钥的模型或保留名称 "local-model"
	// 会被自动启用。
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// UserAgent 是发起 HTTP 请求时使用的 user agent 字符串。
	UserAgent string `json:"user_agent,omitempty" yaml:"-"`

	// isVirtual 标记此模型是由多密钥展开生成的虚拟模型。
	// 虚拟模型不应被持久化到配置文件中。
	isVirtual bool
}

// APIKey 返回 apiKeys 中的第一个 API 密钥
func (c *ModelConfig) APIKey() string {
	if len(c.APIKeys) > 0 {
		return c.APIKeys[0].String()
	}
	return ""
}

// IsVirtual 返回此模型是否由多密钥展开生成。
func (c *ModelConfig) IsVirtual() bool {
	return c.isVirtual
}

// Validate 检查 ModelConfig 是否具备所有必填字段。
func (c *ModelConfig) Validate() error {
	if c.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	if c.Model == "" {
		return fmt.Errorf("model is required")
	}
	if _, err := providercommon.NormalizeToolSchemaTransform(c.ToolSchemaTransform); err != nil {
		return err
	}

	// 拒绝模型标识符中的空白字符
	if strings.ContainsAny(c.Model, " \t\n\r") {
		return fmt.Errorf("model identifier contains whitespace")
	}

	// 拒绝开头的斜杠
	if strings.HasPrefix(c.Model, "/") {
		return fmt.Errorf("model identifier must not start with /")
	}

	// 拒绝连续的斜杠
	if strings.Contains(c.Model, "//") {
		return fmt.Errorf("model identifier must not contain //")
	}
	return nil
}

func (c *ModelConfig) SetAPIKey(value string) {
	if len(c.APIKeys) > 0 {
		c.APIKeys[0].Set(value)
	} else {
		c.APIKeys = append(c.APIKeys, NewSecureString(value))
	}
}

type ToolDiscoveryConfig struct {
	Enabled          bool `json:"enabled"`
	TTL              int  `json:"ttl"`
	MaxSearchResults int  `json:"max_search_results"`
	UseBM25          bool `json:"use_bm25"`
	UseRegex         bool `json:"use_regex"`
}

type ToolConfig struct {
	Enabled bool `json:"enabled" yaml:"-"`
}

type MessageToolsConfig struct {
	ToolConfig `yaml:"-"`

	MediaEnabled bool `json:"media_enabled" yaml:"-"`
}

type BraveConfig struct {
	Enabled    bool          `json:"enabled"           yaml:"-"`
	APIKeys    SecureStrings `json:"api_keys,omitzero" yaml:"api_keys,omitempty"`
	MaxResults int           `json:"max_results"       yaml:"-"`
}

// APIKey 返回 Brave API 密钥
func (c *BraveConfig) APIKey() string {
	if len(c.APIKeys) == 0 {
		return ""
	}
	return c.APIKeys[0].String()
}

// SetAPIKey 设置 Brave API 密钥
func (c *BraveConfig) SetAPIKey(key string) {
	c.APIKeys = SimpleSecureStrings(key)
}

func (c *BraveConfig) SetAPIKeys(keys []string) {
	c.APIKeys = SimpleSecureStrings(keys...)
}

type TavilyConfig struct {
	Enabled    bool          `json:"enabled"           yaml:"-"`
	APIKeys    SecureStrings `json:"api_keys,omitzero" yaml:"api_keys,omitempty"`
	BaseURL    string        `json:"base_url"          yaml:"-"`
	MaxResults int           `json:"max_results"       yaml:"-"`
}

// APIKey 返回 Tavily API 密钥
func (c *TavilyConfig) APIKey() string {
	if len(c.APIKeys) == 0 {
		return ""
	}
	return c.APIKeys[0].String()
}

// SetAPIKey 设置 Tavily API 密钥
func (c *TavilyConfig) SetAPIKey(key string) {
	c.APIKeys = SimpleSecureStrings(key)
}

// SetAPIKeys 设置 Tavily API 密钥列表
func (c *TavilyConfig) SetAPIKeys(keys []string) {
	c.APIKeys = make(SecureStrings, len(keys))
	for i, k := range keys {
		c.APIKeys[i] = NewSecureString(k)
	}
}

type KagiConfig struct {
	Enabled    bool          `json:"enabled"           yaml:"-"`
	APIKeys    SecureStrings `json:"api_keys,omitzero" yaml:"api_keys,omitempty"`
	BaseURL    string        `json:"base_url"          yaml:"-"`
	MaxResults int           `json:"max_results"       yaml:"-"`
}

// APIKey 返回 Kagi API 密钥
func (c *KagiConfig) APIKey() string {
	if len(c.APIKeys) == 0 {
		return ""
	}
	return c.APIKeys[0].String()
}

// SetAPIKey 设置 Kagi API 密钥
func (c *KagiConfig) SetAPIKey(key string) {
	c.APIKeys = SimpleSecureStrings(key)
}

// SetAPIKeys 设置 Kagi API 密钥列表
func (c *KagiConfig) SetAPIKeys(keys []string) {
	c.APIKeys = SimpleSecureStrings(keys...)
}

type DuckDuckGoConfig struct {
	Enabled    bool `json:"enabled"`
	MaxResults int  `json:"max_results"`
}

type SogouConfig struct {
	Enabled    bool `json:"enabled"`
	MaxResults int  `json:"max_results"`
}

type GeminiSearchConfig struct {
	Enabled    bool         `json:"enabled"          yaml:"-"`
	APIKey     SecureString `json:"api_key,omitzero" yaml:"api_key,omitempty"`
	Model      string       `json:"model"            yaml:"-"`
	MaxResults int          `json:"max_results"      yaml:"-"`
}

type PerplexityConfig struct {
	Enabled    bool          `json:"enabled"           yaml:"-"`
	APIKeys    SecureStrings `json:"api_keys,omitzero" yaml:"api_keys,omitempty"`
	MaxResults int           `json:"max_results"       yaml:"-"`
}

// APIKey 返回 Perplexity API 密钥
func (c *PerplexityConfig) APIKey() string {
	if len(c.APIKeys) == 0 {
		return ""
	}
	return c.APIKeys[0].String()
}

// SetAPIKey 设置 Perplexity API 密钥
func (c *PerplexityConfig) SetAPIKey(key string) {
	c.APIKeys = SimpleSecureStrings(key)
}

type SearXNGConfig struct {
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"base_url"`
	MaxResults int    `json:"max_results"`
}

type GLMSearchConfig struct {
	Enabled bool         `json:"enabled"          yaml:"-"`
	APIKey  SecureString `json:"api_key,omitzero" yaml:"api_key,omitempty"`
	BaseURL string       `json:"base_url"         yaml:"-"`
	// SearchEngine 指定搜索后端："search_std"（默认）、
	// "search_pro"、"search_pro_sogou" 或 "search_pro_quark"。
	SearchEngine string `json:"search_engine" yaml:"-"`
	MaxResults   int    `json:"max_results"   yaml:"-"`
}

type BaiduSearchConfig struct {
	Enabled    bool         `json:"enabled"          yaml:"-"`
	APIKey     SecureString `json:"api_key,omitzero" yaml:"api_key,omitempty"`
	BaseURL    string       `json:"base_url"         yaml:"-"`
	MaxResults int          `json:"max_results"      yaml:"-"`
}

type WebToolsConfig struct {
	ToolConfig  `                   yaml:"-"`
	Brave       BraveConfig        `yaml:"brave,omitempty"                                        json:"brave"`
	Tavily      TavilyConfig       `yaml:"tavily,omitempty"                                       json:"tavily"`
	Kagi        KagiConfig         `yaml:"kagi,omitempty"                                         json:"kagi"`
	Sogou       SogouConfig        `yaml:"-"                                                      json:"sogou"`
	DuckDuckGo  DuckDuckGoConfig   `yaml:"-"                                                      json:"duckduckgo"`
	Gemini      GeminiSearchConfig `yaml:"gemini,omitempty"                                       json:"gemini"`
	Perplexity  PerplexityConfig   `yaml:"perplexity,omitempty"                                   json:"perplexity"`
	SearXNG     SearXNGConfig      `yaml:"-"                                                      json:"searxng"`
	GLMSearch   GLMSearchConfig    `yaml:"glm_search,omitempty"                                   json:"glm_search"`
	BaiduSearch BaiduSearchConfig  `yaml:"baidu_search,omitempty"                                 json:"baidu_search"`
	Provider    string             `yaml:"-"                                                      json:"provider,omitempty"`
	// PreferNative 控制当前 LLM 支持时是否使用提供商自带的联网搜索
	// （例如 OpenAI web_search_preview）。为 true 时会隐藏客户端
	// web_search 工具，避免出现重复的搜索入口，而改用提供商内置的搜索。
	// 若提供商不支持内置搜索，则回退到客户端搜索。
	PreferNative bool `yaml:"-" json:"prefer_native"`
	// Proxy 是 web 工具的可选代理 URL（http/https/socks5/socks5h）。
	// 对于需要认证的代理，建议使用 HTTP_PROXY/HTTPS_PROXY 环境变量，而不是把凭据写进配置。
	Proxy                string              `yaml:"-" json:"proxy,omitempty"`
	FetchLimitBytes      int64               `yaml:"-" json:"fetch_limit_bytes,omitempty"`
	Format               string              `yaml:"-" json:"format,omitempty"`
	PrivateHostWhitelist FlexibleStringSlice `yaml:"-" json:"private_host_whitelist,omitempty"`
}

type CronToolsConfig struct {
	ToolConfig `envPrefix:"CHAGENT_TOOLS_CRON_"`
	// 0 表示不设超时。
	ExecTimeoutMinutes    int      `json:"exec_timeout_minutes"`
	AllowCommand          bool     `json:"allow_command"`
	CommandAllowedRemotes []string `json:"command_allowed_remotes"`
}

type ExecConfig struct {
	ToolConfig          ``
	EnableDenyPatterns  bool     `                                 json:"enable_deny_patterns"`
	AllowRemote         bool     `                                 json:"allow_remote"`
	CustomDenyPatterns  []string `                                 json:"custom_deny_patterns"`
	CustomAllowPatterns []string `                                 json:"custom_allow_patterns"`
	TimeoutSeconds      int      `                                 json:"timeout_seconds"` // 0 表示使用默认值（60 秒）
}

// 技能目录投递模式（tools.skills.catalog_mode）。
const (
	// SkillsCatalogModeContext 把技能目录发布为持久上下文，
	// 并通过 `skill` 工具按需加载技能正文（默认）。
	SkillsCatalogModeContext = "context"
	// SkillsCatalogModeSystem 渲染遗留的 `# 技能` system prompt 片段。
	// 在 context 模式推广期间它作为回退保留。
	SkillsCatalogModeSystem = "system"
)

type SkillsToolsConfig struct {
	ToolConfig `                       yaml:"-"`
	Registries SkillsRegistriesConfig `yaml:"registries,omitempty"                                    json:"registries"`
	// Deprecated: 请改用 registries.github。
	Github                SkillsGithubConfig `yaml:"github,omitempty" json:"github"`
	MaxConcurrentSearches int                `yaml:"-"                json:"max_concurrent_searches"`
	SearchCache           SearchCacheConfig  `yaml:"-"                json:"search_cache"`
	// CatalogMode 选择已安装技能目录如何到达模型：
	// "context"（持久上下文消息，渐进披露；默认）或 "system"（遗留完整 system prompt 片段）。
	CatalogMode string `yaml:"-" json:"catalog_mode,omitempty"`
}

// ApplyDefaults 填充技能目录模式的默认值（"context"）。
func (c *SkillsToolsConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.CatalogMode) == "" {
		c.CatalogMode = SkillsCatalogModeContext
	}
}

// EffectiveCatalogMode 返回规范化后的目录投递模式。
// 空值或未知值回退到默认 "context" 模式；
// 配置校验在加载时拒绝未知值。
func (c *SkillsToolsConfig) EffectiveCatalogMode() string {
	if c == nil {
		return SkillsCatalogModeContext
	}
	switch strings.ToLower(strings.TrimSpace(c.CatalogMode)) {
	case SkillsCatalogModeSystem:
		return SkillsCatalogModeSystem
	default:
		return SkillsCatalogModeContext
	}
}

// Validate 校验技能目录模式。
func (c *SkillsToolsConfig) Validate() error {
	if c == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(c.CatalogMode))
	if mode != "" && mode != SkillsCatalogModeContext && mode != SkillsCatalogModeSystem {
		return fmt.Errorf(
			"tools.skills.catalog_mode = %q is invalid: allowed values are %q or %q",
			c.CatalogMode,
			SkillsCatalogModeContext,
			SkillsCatalogModeSystem,
		)
	}
	return nil
}

type MediaCleanupConfig struct {
	ToolConfig ``
	MaxAge     int `                                    json:"max_age_minutes"`
	Interval   int `                                    json:"interval_minutes"`
}

type ReadFileToolConfig struct {
	Enabled         bool   `json:"enabled"`
	Mode            string `json:"mode"`
	MaxReadFileSize int    `json:"max_read_file_size"`
}

const (
	ReadFileModeBytes = "bytes"
	ReadFileModeLines = "lines"
)

func (c ReadFileToolConfig) EffectiveMode() string {
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case ReadFileModeLines:
		return ReadFileModeLines
	case "", ReadFileModeBytes:
		return ReadFileModeBytes
	default:
		return ReadFileModeBytes
	}
}

type ToolsConfig struct {
	AllowReadPaths  []string `json:"allow_read_paths"  yaml:"-"`
	AllowWritePaths []string `json:"allow_write_paths" yaml:"-"`
	// FilterSensitiveData 控制是否在把工具结果发送给 LLM 之前
	// 过滤其中的敏感值（API 密钥、token、secret）。
	// 默认：true（启用）
	FilterSensitiveData bool `json:"filter_sensitive_data" yaml:"-"`
	// FilterMinLength 是执行过滤所需的最小内容长度。
	// 出于性能考虑，短于该长度的内容将原样返回。
	// 默认：8
	FilterMinLength int                `json:"filter_min_length" yaml:"-"`
	Web             WebToolsConfig     `json:"web"               yaml:"web,omitempty"`
	Cron            CronToolsConfig    `json:"cron"              yaml:"-"`
	Exec            ExecConfig         `json:"exec"              yaml:"-"`
	Skills          SkillsToolsConfig  `json:"skills"            yaml:"skills,omitempty"`
	MediaCleanup    MediaCleanupConfig `json:"media_cleanup"     yaml:"-"`
	MCP             MCPConfig          `json:"mcp"               yaml:"-"`
	AppendFile      ToolConfig         `json:"append_file"       yaml:"-"`
	EditFile        ToolConfig         `json:"edit_file"         yaml:"-"`
	FindSkills      ToolConfig         `json:"find_skills"       yaml:"-"`
	InstallSkill    ToolConfig         `json:"install_skill"     yaml:"-"`
	ListDir         ToolConfig         `json:"list_dir"          yaml:"-"`
	LoadImage       ToolConfig         `json:"load_image"        yaml:"-"`
	Message         MessageToolsConfig `json:"message"           yaml:"-"`
	ReadFile        ReadFileToolConfig `json:"read_file"         yaml:"-"`
	SendFile        ToolConfig         `json:"send_file"         yaml:"-"`
	Spawn           ToolConfig         `json:"spawn"             yaml:"-"`
	SpawnStatus     ToolConfig         `json:"spawn_status"      yaml:"-"`
	Subagent        ToolConfig         `json:"subagent"          yaml:"-"`
	Task            ToolConfig         `json:"task"              yaml:"-"`
	Todo            ToolConfig         `json:"todo"              yaml:"-"`
	SkillLoad       ToolConfig         `json:"skill_load"        yaml:"-"`
	WebFetch        ToolConfig         `json:"web_fetch"         yaml:"-"`
	WriteFile       ToolConfig         `json:"write_file"        yaml:"-"`
	// Reaction 控制消息表情回应工具；未配置时禁用（严格按配置列表启用工具）。
	Reaction ToolConfig `json:"reaction" yaml:"-"`
	// DSH 是独立的 DSH 工具组（glob/grep、jobs、MCP 资源、
	// 会话查询、子代理控制、持久化目标）。参见 dsh_tools_config.go。
	DSH DSHGroupConfig `json:"dsh" yaml:"-"`
}

// IsFilterSensitiveDataEnabled 在启用敏感数据过滤时返回 true
func (c *ToolsConfig) IsFilterSensitiveDataEnabled() bool {
	return c.FilterSensitiveData
}

// GetFilterMinLength 返回过滤所需的最小内容长度（默认：8）
func (c *ToolsConfig) GetFilterMinLength() int {
	if c.FilterMinLength <= 0 {
		return 8
	}
	return c.FilterMinLength
}

type SearchCacheConfig struct {
	MaxSize    int `json:"max_size"`
	TTLSeconds int `json:"ttl_seconds"`
}

type SkillsRegistriesConfig []*SkillRegistryConfig

func (c *SkillsRegistriesConfig) Get(name string) (SkillRegistryConfig, bool) {
	if c == nil {
		return SkillRegistryConfig{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return SkillRegistryConfig{}, false
	}
	for _, registry := range *c {
		if registry == nil || registry.Name != name {
			continue
		}
		return *registry, true
	}
	return SkillRegistryConfig{}, false
}

func (c *SkillsRegistriesConfig) Set(name string, cfg SkillRegistryConfig) {
	if c == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	cfg.Name = name
	for i, registry := range *c {
		if registry == nil || registry.Name != name {
			continue
		}
		(*c)[i] = &cfg
		return
	}
	*c = append(*c, &cfg)
}

type SkillsGithubConfig struct {
	BaseURL string       `json:"base_url,omitempty" yaml:"-"`
	Token   SecureString `json:"token,omitzero"     yaml:"token,omitempty"`
	Proxy   string       `json:"proxy,omitempty"    yaml:"-"`
}

type SkillRegistryConfig struct {
	Name      string         `json:"name,omitempty"      yaml:"-"`
	Enabled   bool           `json:"enabled"             yaml:"-"`
	BaseURL   string         `json:"base_url"            yaml:"-"`
	AuthToken SecureString   `json:"auth_token,omitzero" yaml:"auth_token,omitempty"`
	Param     map[string]any `json:"-"                   yaml:"-"`
}

const (
	envSkillsClawHubEnabled         = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_ENABLED"
	envSkillsClawHubBaseURL         = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_BASE_URL"
	envSkillsClawHubAuthToken       = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_AUTH_TOKEN"
	envSkillsClawHubSearchPath      = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_SEARCH_PATH"
	envSkillsClawHubSkillsPath      = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_SKILLS_PATH"
	envSkillsClawHubDownloadPath    = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_DOWNLOAD_PATH"
	envSkillsClawHubTimeout         = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_TIMEOUT"
	envSkillsClawHubMaxZipSize      = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_MAX_ZIP_SIZE"
	envSkillsClawHubMaxResponseSize = "CHAGENT_SKILLS_REGISTRIES_CLAWHUB_MAX_RESPONSE_SIZE"
	envSkillsGitHubEnabled          = "CHAGENT_SKILLS_REGISTRIES_GITHUB_ENABLED"
	envSkillsGitHubBaseURL          = "CHAGENT_SKILLS_REGISTRIES_GITHUB_BASE_URL"
	envSkillsGitHubAuthToken        = "CHAGENT_SKILLS_REGISTRIES_GITHUB_AUTH_TOKEN"
	envSkillsGitHubProxy            = "CHAGENT_SKILLS_REGISTRIES_GITHUB_PROXY"
)

func (c *SkillRegistryConfig) DecodeParam(target any) error {
	if c == nil {
		return nil
	}
	if len(c.Param) == 0 {
		return nil
	}
	data, err := json.Marshal(c.Param)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// MCPServerConfig 定义单个 MCP 服务器的配置
type MCPServerConfig struct {
	// Enabled 表示此 MCP 服务器是否启用
	Enabled bool `json:"enabled"`
	// Deferred 控制此服务器的工具是否以隐藏方式注册（延迟/发现模式）。
	// 为 nil 时，应用全局的 Discovery.Enabled 设置。
	// 显式设为 true 或 false 时，仅对此服务器覆盖全局设置。
	Deferred *bool `json:"deferred,omitempty"`
	// Command 是要运行的可执行文件（例如 "npx"、"python"、"/path/to/server"）
	Command string `json:"command"`
	// Args 是传递给命令的参数
	Args []string `json:"args,omitempty"`
	// Env 是为服务器进程设置的环境变量（仅 stdio）
	Env map[string]string `json:"env,omitempty"`
	// EnvFile 是包含环境变量的文件路径（仅 stdio）
	EnvFile string `json:"env_file,omitempty"`
	// Type 取值为 "stdio"、"sse"、"http" 或 "streamable-http"。
	// "http" 与 "streamable-http" 都会选择可流式的 HTTP 请求-响应模式，
	// 而 "sse" 会保留独立的 SSE 监听，以接收服务器主动发起的通知。
	// 默认值：设置了 command 则为 stdio，设置了 url 则为 sse。
	Type string `json:"type,omitempty"`
	// URL 用于 SSE/HTTP 传输
	URL string `json:"url,omitempty"`
	// Headers 是随请求发送的 HTTP 头（仅 sse/http）
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig 定义所有 MCP 服务器的配置
type MCPConfig struct {
	ToolConfig ``
	Discovery  ToolDiscoveryConfig `                                json:"discovery"`
	// MaxInlineTextChars 控制 MCP 文本在保存为产物之前保留多少内联内容。
	MaxInlineTextChars int `json:"max_inline_text_chars,omitempty"`
	// KeepAliveInterval 控制发送 ping 消息以保持连接存活的频率。
	// 若未设置或设为 0，则使用 30 秒的默认值。
	// 设为负值可禁用 keepalive。
	KeepAliveInterval int `json:"keepalive_interval,omitempty"`
	// Servers 是服务器名称到服务器配置的映射
	Servers map[string]MCPServerConfig `json:"servers,omitempty"`
	// ExplicitDisabled 记录配置文件是否显式设置了 tools.mcp.enabled=false。
	// LoadConfig 会从原始 JSON 中填充该字段；它永不参与序列化。
	// 它使显式关闭与省略 enabled 标志两种情况可区分，因此只配置 servers 的文件仍会自动启用。
	ExplicitDisabled bool `json:"-"`
}

const DefaultMCPMaxInlineTextChars = 16 * 1024

// DefaultMCPKeepAliveInterval 是默认的 keepalive 间隔（秒）。
const DefaultMCPKeepAliveInterval = 30 // 30 秒

// Active 报告 MCP 是否应当运行。显式设置 tools.mcp.enabled=false
// 时，即使配置了服务器也会关闭 MCP；否则只要配置了服务器即可，
// 遗留的 mcp.enabled 标志对 chess 场景仍保持可选。
func (c *MCPConfig) Active() bool {
	if c == nil {
		return false
	}
	if c.ExplicitDisabled {
		return false
	}
	return c.Enabled || len(c.Servers) > 0
}

func (c *MCPConfig) GetMaxInlineTextChars() int {
	if c.MaxInlineTextChars > 0 {
		return c.MaxInlineTextChars
	}
	return DefaultMCPMaxInlineTextChars
}

// GetKeepAliveInterval 返回 keepalive 间隔（秒）。
// 若 keepalive 已被禁用（负值），返回 0。
// 若未设置，返回默认值（30 秒）。
func (c *MCPConfig) GetKeepAliveInterval() int {
	if c.KeepAliveInterval < 0 {
		return 0 // 已禁用
	}
	if c.KeepAliveInterval == 0 {
		return DefaultMCPKeepAliveInterval
	}
	return c.KeepAliveInterval
}

func LoadConfig(path string) (*Config, error) {
	updateResolver(filepath.Dir(path))

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.WarnF(
				"config file not found, using default config",
				map[string]any{"path": path},
			)
			return DefaultConfig(), nil
		}
		return nil, err
	}

	// 规范配置使用单数形式的 api_key。在解码之前先把它
	// 规范化为内部使用的单元素密钥列表。
	if normalized, normErr := normalizeSingularAPIKeys(data); normErr != nil {
		return nil, normErr
	} else {
		data = normalized
	}

	// 首先，尝试通过读取 version 字段来检测配置版本
	var versionInfo struct {
		Version int `json:"version"`
	}
	if e := json.Unmarshal(data, &versionInfo); e != nil {
		e = wrapJSONError(data, e, "config.json")
		logger.ErrorCF("config", formatDiagnosticLogMessage("Malformed config file", e), map[string]any{"path": path})
		return nil, e
	}
	if len(data) <= 10 {
		logger.Warn(fmt.Sprintf("content is [%s]", string(data)))
		return DefaultConfig(), nil
	}

	// 仅支持规范的 v2 JSON 配置。旧版本以及就地迁移逻辑
	// 已被移除；chess 直接写入这种格式。
	if versionInfo.Version != CurrentVersion {
		return nil, fmt.Errorf(
			"unsupported config version: %d (only version %d is supported)",
			versionInfo.Version, CurrentVersion,
		)
	}
	var cfg *Config
	cfg, err = loadConfig(data)
	if err != nil {
		logger.ErrorCF(
			"config",
			formatDiagnosticLogMessage("Failed to load config", err),
			map[string]any{"path": path},
		)
		return nil, err
	}
	// 保留“显式关闭”的语义：tools.mcp.enabled=false 会关闭 MCP，
	// 即使已经配置了服务器；而省略 enabled 则保留“仅配置服务器即自动启用”的路径
	// （参见 MCPConfig.Active）。该标记从原始 JSON 推导而来，且永不参与序列化。
	cfg.Tools.MCP.ExplicitDisabled = rawMCPExplicitlyDisabled(data)
	if err = InitChannelList(cfg.Channels); err != nil {
		return nil, err
	}
	if agentName, ok := os.LookupEnv(AgentNameEnv); ok {
		if tokenErr := ApplyNameDerivedPicoToken(cfg, agentName); tokenErr != nil {
			logger.WarnCF("config", "failed to apply name-derived pico token", map[string]any{
				"error": tokenErr.Error(),
			})
		}
	}
	if err = cfg.ValidateTurnProfile(); err != nil {
		return nil, err
	}
	// 校验 model_list 的唯一性和必填字段
	if err = cfg.ValidateModelList(); err != nil {
		return nil, err
	}

	// 在 ApplyContextDefaults 重写这些值之前，先拒绝显式配置的非正数
	// context/autonomy 参数，这样像 max_rounds: 0 这样的笔误就能被诊断出来，
	// 而不是静默地变成默认值。省略的字段不受影响，仍会在下文获得 chess 配置档的默认值。
	if err = ValidateExplicitContextZeros(data); err != nil {
		return nil, err
	}

	// chess 配置档的默认值在加载后应用，这样省略新分段的旧配置
	// 仍可正常工作。
	cfg.ApplyContextDefaults()
	if err = cfg.ValidateContextConfigs(); err != nil {
		return nil, err
	}
	// 技能目录投递默认为 "context"（T4.3 渐进披露）。
	cfg.Tools.Skills.ApplyDefaults()
	if err = cfg.Tools.Skills.Validate(); err != nil {
		return nil, err
	}

	// 确保未设置时 Workspace 有默认值。chess 会设置
	// CHAGENT_WORKSPACE；工作区中的裸配置文件也会让该目录
	// 成为 chess/client 期望的 pid 文件位置。
	if !hasExplicitWorkspace(data) {
		// 配置文件（或环境变量）没有指定工作区。改用 chess 的回退链，
		// 而不是 DefaultConfig 的安装目录工作区，这样 agent 工作区中的
		// 裸 config.json 会继续把 pid 文件放在该目录。
		cfg.Agents.Defaults.Workspace = defaultWorkspaceChain(path)
	}

	// 当 chess 省略默认模型/提供商时，推断出可用的默认值。
	if err = inferDefaultModel(cfg); err != nil {
		return nil, err
	}
	inferDefaultProvider(cfg)

	cfg.Session.ApplyDmScope()
	cfg.Session.DeriveDmScope()

	// 加载时不要重写文件。chess/client 会直接从 config.json 读取
	// channels.pico.token；而 Chagent 的 SecureString 序列化会在重写文件时
	// 省略它（它存放在 .security.yml 中），这会破坏 chess 的下一次启动。
	// 因此迁移只保留在内存中。

	return cfg, nil
}

func MakeBackup(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	dateSuffix := time.Now().Format(".20060102.bak")
	// 备份配置文件
	bakPath := path + dateSuffix
	if err := fileutil.CopyFile(path, bakPath, 0o600); err != nil {
		logger.ErrorF("failed to create config backup", map[string]any{"error": err})
		return fmt.Errorf("failed to create config backup: %w", err)
	}
	return nil
}

func toNameIndex(list []*ModelConfig) []string {
	nameList := make([]string, 0, len(list))
	countMap := make(map[string]int)
	for _, model := range list {
		name := model.ModelName
		index := countMap[name]
		nameList = append(nameList, fmt.Sprintf("%s:%d", name, index))
		countMap[name]++
	}
	return nameList
}

func SaveConfig(path string, cfg *Config) error {
	if cfg.Version < CurrentVersion {
		cfg.Version = CurrentVersion
	}
	// 序列化到配置文件之前先过滤掉虚拟模型
	nonVirtualModels := make([]*ModelConfig, 0, len(cfg.ModelList))
	for _, m := range cfg.ModelList {
		if !m.isVirtual {
			nonVirtualModels = append(nonVirtualModels, m)
		}
	}
	// 序列化时临时用过滤后的版本替换 ModelList
	originalModelList := cfg.ModelList
	defer func() {
		// 序列化完成后恢复原始的 ModelList
		cfg.ModelList = originalModelList
	}()
	cfg.ModelList = nonVirtualModels

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data, err = canonicalizeSavedConfig(data, cfg)
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(path, data, 0o600)
}

// canonicalizeSavedConfig 写入唯一受支持的配置模式：单数形式
// api_key、扁平化的 pico token 以及 v2 的 channels 键。SecureString 序列化
// 会有意省略密钥，因此这里是唯一会把已配置的值重新写回规范 JSON 的地方。
func canonicalizeSavedConfig(data []byte, cfg *Config) ([]byte, error) {
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	if entries, ok := top["model_list"].([]any); ok {
		for i, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok || i >= len(cfg.ModelList) || cfg.ModelList[i] == nil {
				continue
			}
			delete(entry, "api_keys")
			if key := cfg.ModelList[i].APIKey(); key != "" {
				entry["api_key"] = key
			}
		}
	}
	if pico := cfg.Channels.Get("pico"); pico != nil {
		if decoded, err := pico.GetDecoded(); err == nil {
			if settings, ok := decoded.(*PicoSettings); ok && settings.Token.String() != "" {
				channels, _ := top["channels"].(map[string]any)
				if channels == nil {
					channels = map[string]any{}
					top["channels"] = channels
				}
				picoRaw, _ := channels["pico"].(map[string]any)
				if picoRaw == nil {
					picoRaw = map[string]any{}
					channels["pico"] = picoRaw
				}
				picoRaw["token"] = settings.Token.String()
			}
		}
	}
	encoded, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func (c *Config) WorkspacePath() string {
	return expandHome(c.Agents.Defaults.Workspace)
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}

// GetModelConfig 返回给定模型名称对应的 ModelConfig。
// 若存在多个相同 model_name 的配置，则使用轮询选择以实现负载均衡。
// 模型未找到时返回错误。
func (c *Config) GetModelConfig(modelName string) (*ModelConfig, error) {
	matches := c.findMatches(modelName)
	if len(matches) == 0 {
		return nil, fmt.Errorf("model %q not found in model_list or providers", modelName)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	// 多个配置——使用轮询实现负载均衡
	idx := (rrCounter.Add(1) - 1) % uint64(len(matches))
	return matches[idx], nil
}

// findMatches 查找所有 model_name 匹配的 ModelConfig 条目。
func (c *Config) findMatches(modelName string) []*ModelConfig {
	var matches []*ModelConfig
	for i := range c.ModelList {
		if c.ModelList[i].ModelName == modelName {
			matches = append(matches, c.ModelList[i])
		}
	}
	return matches
}

// ValidateModelList 校验 model_list 中的所有 ModelConfig 条目。
// 它会检查每个模型配置是否有效。
// 注意：出于负载均衡考虑，允许存在多个相同 model_name 的条目。
func (c *Config) ValidateModelList() error {
	for i := range c.ModelList {
		if err := c.ModelList[i].Validate(); err != nil {
			return fmt.Errorf("model_list[%d]: %w", i, err)
		}
	}
	return nil
}

// ResetToDefaults 备份当前配置，创建默认配置，
// 保留现有配置中的安全凭据，然后保存。
func ResetToDefaults(configPath string) error {
	if err := MakeBackup(configPath); err != nil {
		return fmt.Errorf("backup before reset: %w", err)
	}
	cfg := DefaultConfig()
	cfg.Session.ApplyDmScope()
	cfg.Session.DeriveDmScope()
	return SaveConfig(configPath, cfg)
}

func expandMultiKeyModels(models []*ModelConfig) []*ModelConfig {
	var expanded []*ModelConfig

	for _, m := range models {
		keys := m.APIKeys.Values()

		// 单个密钥或没有密钥：保持原样
		if len(keys) <= 1 {
			expanded = append(expanded, m)
			continue
		}

		// 多个密钥：进行展开
		originalName := m.ModelName

		// 为额外密钥创建条目（key_1、key_2、……）
		var fallbackNames []string
		for i := 1; i < len(keys); i++ {
			suffix := fmt.Sprintf("__key_%d", i)
			expandedName := originalName + suffix

			// 为额外密钥创建副本
			additionalEntry := &ModelConfig{
				ModelName:           expandedName,
				Provider:            m.Provider,
				Model:               m.Model,
				APIBase:             m.APIBase,
				APIKeys:             SimpleSecureStrings(keys[i]),
				Proxy:               m.Proxy,
				Workspace:           m.Workspace,
				RPM:                 m.RPM,
				MaxTokensField:      m.MaxTokensField,
				RequestTimeout:      m.RequestTimeout,
				ThinkingLevel:       m.ThinkingLevel,
				ToolSchemaTransform: m.ToolSchemaTransform,
				Streaming:           m.Streaming,
				ExtraBody:           m.ExtraBody,
				CustomHeaders:       m.CustomHeaders,
				UserAgent:           m.UserAgent,
				isVirtual:           true,
			}
			expanded = append(expanded, additionalEntry)
			fallbackNames = append(fallbackNames, expandedName)
		}

		// 用第一个密钥和回退列表创建主条目
		primaryEntry := &ModelConfig{
			ModelName:           originalName,
			Provider:            m.Provider,
			Model:               m.Model,
			APIBase:             m.APIBase,
			Proxy:               m.Proxy,
			Workspace:           m.Workspace,
			RPM:                 m.RPM,
			MaxTokensField:      m.MaxTokensField,
			RequestTimeout:      m.RequestTimeout,
			ThinkingLevel:       m.ThinkingLevel,
			ToolSchemaTransform: m.ToolSchemaTransform,
			Streaming:           m.Streaming,
			ExtraBody:           m.ExtraBody,
			CustomHeaders:       m.CustomHeaders,
			UserAgent:           m.UserAgent,
			APIKeys:             SimpleSecureStrings(keys[0]),
		}

		// 将新的回退项添加到已有回退项之前
		if len(fallbackNames) > 0 {
			primaryEntry.Fallbacks = append(fallbackNames, m.Fallbacks...)
		} else if len(m.Fallbacks) > 0 {
			primaryEntry.Fallbacks = m.Fallbacks
		}

		expanded = append(expanded, primaryEntry)
	}

	return expanded
}

func (t *ToolsConfig) IsToolEnabled(name string) bool {
	switch name {
	case "web":
		return t.Web.Enabled
	case "cron":
		return t.Cron.Enabled
	case "exec":
		return t.Exec.Enabled
	case "skills":
		return t.Skills.Enabled
	case "media_cleanup":
		return t.MediaCleanup.Enabled
	case "append_file":
		return t.AppendFile.Enabled
	case "edit_file":
		return t.EditFile.Enabled
	case "find_skills":
		return t.FindSkills.Enabled
	case "install_skill":
		return t.InstallSkill.Enabled
	case "list_dir":
		return t.ListDir.Enabled
	case "load_image":
		return t.LoadImage.Enabled
	case "message":
		return t.Message.Enabled
	case "read_file":
		return t.ReadFile.Enabled
	case "spawn":
		return t.Spawn.Enabled
	case "spawn_status":
		return t.SpawnStatus.Enabled
	case "subagent":
		return t.Subagent.Enabled
	case "task", "task_get", "task_update":
		return t.Task.Enabled
	case "todo", "todo_write":
		return t.Todo.Enabled
	case "skill", "skill_load", "skill_view":
		return t.SkillLoad.Enabled
	case "web_fetch":
		return t.WebFetch.Enabled
	case "send_file":
		return t.SendFile.Enabled
	case "write_file":
		return t.WriteFile.Enabled
	case "mcp":
		return t.MCP.Active()
	case "dsh":
		return t.DSH.IsEnabled()
	case "reaction":
		return t.Reaction.Enabled
	default:
		return true
	}
}
