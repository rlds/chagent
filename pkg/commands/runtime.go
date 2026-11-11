package commands

import (
	"context"

	"github.com/rlds/chagent/pkg/config"
)

type MCPServerInfo struct {
	Name      string
	Enabled   bool
	Deferred  bool
	Connected bool
	ToolCount int
}

type MCPToolParameterInfo struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

type MCPToolInfo struct {
	Name        string
	Description string
	Parameters  []MCPToolParameterInfo
}

// ContextStats 描述当前会话上下文窗口的使用情况。
type ContextStats struct {
	UsedTokens        int
	TotalTokens       int // 模型上下文窗口
	HistoryTokens     int // 仅历史部分的 token 数（maybeSummarize 所检查的对象）
	CompressAtTokens  int // 硬预算压缩阈值
	SummarizeAtTokens int // 软性摘要触发阈值
	UsedPercent       int // 0-100
	MessageCount      int

	// 工作区指令上下文（T4.4）。禁用或无基线时为 nil。
	Workspace *WorkspaceContextStats
	// 运行时快照状态（T4.2）。
	Runtime *RuntimeContextStats
	// 持久任务状态（P-09/T5.3）。
	Task *TaskContextStats
}

type WorkspaceContextStats struct {
	Enabled    bool
	Baseline   bool
	Files      int
	Bytes      int
	Budget     int
	UserGlobal int
	Project    int
	Omitted    int
	Truncated  int
	Paths      []string
}

type RuntimeContextStats struct {
	Mode     string
	Interval int // 秒
	Snapshot string
}

type TaskContextStats struct {
	Phase      string
	Round      int
	MaxRounds  int
	Todos      int
	InProgress int
}

// StopResult 描述当前会话停止请求的结果。
type StopResult struct {
	Stopped  bool
	TaskName string
}

// Runtime 为命令处理器提供运行时依赖。它由 agent 循环按请求构建，
// 以便按请求变化的状态（例如会话作用域）能与长期存活的回调
// （例如 GetModelInfo）共存。
type Runtime struct {
	Config             *config.Config
	GetModelInfo       func() (name, provider string)
	AskSideQuestion    func(ctx context.Context, question string) (string, error)
	ListAgentIDs       func() []string
	ListDefinitions    func() []Definition
	ListSkillNames     func() []string
	ListMCPServers     func(ctx context.Context) []MCPServerInfo
	ListMCPTools       func(ctx context.Context, serverName string) ([]MCPToolInfo, error)
	GetEnabledChannels func() []string
	GetActiveTurn      func() any // 返回 any，以避免与 agent 包产生循环依赖
	GetContextStats    func() *ContextStats
	SwitchModel        func(value string) (oldModel string, err error)
	SwitchChannel      func(value string) error
	ClearHistory       func() error
	ReloadConfig       func() error
	StopActiveTurn     func() (StopResult, error)
}
