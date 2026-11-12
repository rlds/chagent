package channels

import "context"

// ExecutionReportSender 是通道可选实现的扩展接口：把一次工具执行的上报
// 以渠道原生协议发给对端。
//
// 目前由 pico（gateway 模式）与 pico_client（客户端模式）实现，线上消息类型
// 为 `execution.report`。渠道不实现该接口时，agent 侧只记录一条 debug 日志，
// 绝不影响工具结果或主循环。
//
// 实现约束：
//   - 必须是非阻塞语义上的"尽力而为"：调用方在独立的发送 goroutine 中调用它，
//     但仍应在没有可用连接时快速返回错误，而不是等待连接；
//   - 返回错误只能用于日志，调用方不得把它传播给工具执行路径。
type ExecutionReportSender interface {
	// SendExecutionReport 向 chatID 对应的会话发送一条 execution.report。
	// payload 已经由调用方裁剪与脱敏（不含完整工具结果或大参数明文）。
	SendExecutionReport(ctx context.Context, chatID string, payload map[string]any) error
}
