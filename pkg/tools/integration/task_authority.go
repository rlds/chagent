package integrationtools

import "context"

// Source 描述当前轮次的来源。task 工具用它来约束：创建任务必须来自
// 直接的人类请求或调度器请求，恢复被阻塞的任务必须来自直接的人类请求。
type Source string

const (
	// SourceHuman 是直接收到的人类消息。
	SourceHuman Source = "human"
	// SourceScheduler 是由心跳/cron 触发的轮次。
	SourceScheduler Source = "scheduler"
	// SourceAutonomy 是由自主驱动器调度的续跑轮次。
	SourceAutonomy Source = "autonomy"
	// SourceSubagent 是在 subagent 内运行的轮次。
	SourceSubagent Source = "subagent"
	// SourceSystem 是其他任何非人类、非调度器的轮次。
	SourceSystem Source = "system"
	// SourceUnknown 表示轮次来源未被上报。
	SourceUnknown Source = "unknown"
)

func (s Source) String() string {
	if s == "" {
		return string(SourceUnknown)
	}
	return string(s)
}

// AllowsStart 报告该来源是否可以创建新任务。
func (s Source) AllowsStart() bool {
	return s == SourceHuman || s == SourceScheduler
}

// AllowsResumePaused 报告该来源是否可以恢复已暂停的任务。
func (s Source) AllowsResumePaused() bool {
	return s.AllowsStart()
}

// AllowsResumeBlocked 报告该来源是否可以恢复被阻塞的任务。
// 恢复被阻塞的任务始终需要直接的人类请求。
func (s Source) AllowsResumeBlocked() bool {
	return s == SourceHuman
}

// Authority 为 task 工具解析当前轮次的来源。
//
// agent 循环通过 WithTurnSource 将具体的轮次来源注入工具上下文；
// 以其他方式跟踪轮次来源的部署可以实现此接口，
// 并通过 TaskToolOptions.Authority 传入。
type Authority interface {
	Source(ctx context.Context) Source
}

// AuthorityFunc 将函数适配为 Authority 接口。
type AuthorityFunc func(ctx context.Context) Source

// Source 实现 Authority。
func (f AuthorityFunc) Source(ctx context.Context) Source {
	if f == nil {
		return SourceUnknown
	}
	return f(ctx)
}

type turnSourceCtxKey struct{}

// WithTurnSource 返回携带轮次来源的子上下文。
func WithTurnSource(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, turnSourceCtxKey{}, source)
}

// TurnSourceFromContext 提取通过 WithTurnSource 注入的轮次来源。
func TurnSourceFromContext(ctx context.Context) Source {
	if ctx == nil {
		return SourceUnknown
	}
	source, ok := ctx.Value(turnSourceCtxKey{}).(Source)
	if !ok || source == "" {
		return SourceUnknown
	}
	return source
}

// DefaultAuthority 报告注入工具上下文的轮次来源。
// 未知来源永远不会授权创建任务或恢复阻塞任务，
// 因此 agent 循环必须为人类轮次和调度器轮次注入 WithTurnSource。
type DefaultAuthority struct{}

// Source 实现 Authority。
func (DefaultAuthority) Source(ctx context.Context) Source {
	return TurnSourceFromContext(ctx)
}
