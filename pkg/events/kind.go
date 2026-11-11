package events

const (
	// KindAgentTurnStart 在 agent 轮次开始时发出。
	KindAgentTurnStart Kind = "agent.turn.start"
	// KindAgentTurnEnd 在 agent 轮次结束时发出。
	KindAgentTurnEnd Kind = "agent.turn.end"

	// KindAgentLLMRequest 在发起 LLM 请求之前发出。
	KindAgentLLMRequest Kind = "agent.llm.request"
	// KindAgentLLMDelta 在流式输出 LLM 增量时发出。
	KindAgentLLMDelta Kind = "agent.llm.delta"
	// KindAgentLLMResponse 在收到 LLM 响应之后发出。
	KindAgentLLMResponse Kind = "agent.llm.response"
	// KindAgentLLMRetry 在重试 LLM 请求之前发出。
	KindAgentLLMRetry Kind = "agent.llm.retry"

	// KindAgentContextCompress 在 agent 上下文被压缩时发出。
	KindAgentContextCompress Kind = "agent.context.compress"
	// KindAgentSessionSummarize 在会话摘要完成时发出。
	KindAgentSessionSummarize Kind = "agent.session.summarize"

	// KindAgentToolExecStart 在工具执行之前发出。
	KindAgentToolExecStart Kind = "agent.tool.exec_start"
	// KindAgentToolExecEnd 在工具执行完成之后发出。
	KindAgentToolExecEnd Kind = "agent.tool.exec_end"
	// KindAgentToolExecSkipped 在工具调用被跳过时发出。
	KindAgentToolExecSkipped Kind = "agent.tool.exec_skipped"

	// KindAgentSteeringInjected 在 steering 指令被注入上下文时发出。
	KindAgentSteeringInjected Kind = "agent.steering.injected"
	// KindAgentAutonomyRound 在自主任务轮次被 autonomy 驱动调度、
	// 开始或结束时发出。
	KindAgentAutonomyRound Kind = "agent.autonomy.round"
	// KindAgentFollowUpQueued 在异步后续输入进入队列时发出。
	KindAgentFollowUpQueued Kind = "agent.follow_up.queued"
	// KindAgentInterruptReceived 在轮次中断请求被接受时发出。
	KindAgentInterruptReceived Kind = "agent.interrupt.received"

	// KindAgentSubTurnSpawn 在创建子轮次时发出。
	KindAgentSubTurnSpawn Kind = "agent.subturn.spawn"
	// KindAgentSubTurnEnd 在子轮次结束时发出。
	KindAgentSubTurnEnd Kind = "agent.subturn.end"
	// KindAgentSubTurnResultDelivered 在子轮次结果被投递时发出。
	KindAgentSubTurnResultDelivered Kind = "agent.subturn.result_delivered"
	// KindAgentSubTurnOrphan 在子轮次结果无法投递时发出。
	KindAgentSubTurnOrphan Kind = "agent.subturn.orphan"
	// KindAgentError 在 agent 执行报告错误时发出。
	KindAgentError Kind = "agent.error"

	// KindChannelLifecycleStarted 在 channel 启动时发出。
	KindChannelLifecycleStarted Kind = "channel.lifecycle.started"
	// KindChannelLifecycleInitialized 在 channel 初始化完成时发出。
	KindChannelLifecycleInitialized Kind = "channel.lifecycle.initialized"
	// KindChannelLifecycleStartFailed 在 channel 启动失败时发出。
	KindChannelLifecycleStartFailed Kind = "channel.lifecycle.start_failed"
	// KindChannelLifecycleStopped 在 channel 停止时发出。
	KindChannelLifecycleStopped Kind = "channel.lifecycle.stopped"
	// KindChannelWebhookRegistered 在 channel webhook 注册完成时发出。
	KindChannelWebhookRegistered Kind = "channel.webhook.registered"
	// KindChannelWebhookUnregistered 在 channel webhook 注销完成时发出。
	KindChannelWebhookUnregistered Kind = "channel.webhook.unregistered"
	// KindChannelMessageOutboundQueued 在出站消息进入队列时发出。
	KindChannelMessageOutboundQueued Kind = "channel.message.outbound_queued"
	// KindChannelMessageOutboundSent 在 channel 出站消息发送成功时发出。
	KindChannelMessageOutboundSent Kind = "channel.message.outbound_sent"
	// KindChannelMessageOutboundFailed 在 channel 出站消息发送失败时发出。
	KindChannelMessageOutboundFailed Kind = "channel.message.outbound_failed"
	// KindChannelRateLimited 在 channel 限流导致投递被阻塞时发出。
	KindChannelRateLimited Kind = "channel.rate_limited"

	// KindBusPublishFailed 在消息总线发布失败时发出。
	KindBusPublishFailed Kind = "bus.publish.failed"
	// KindBusMessageDropped 在消息因背压（channel 满载时间超过丢弃预算）
	// 被丢弃时发出。
	KindBusMessageDropped Kind = "bus.message.dropped"
	// KindBusCloseStarted 在消息总线开始关闭时发出。
	KindBusCloseStarted Kind = "bus.close.started"
	// KindBusCloseCompleted 在消息总线关闭完成时发出。
	KindBusCloseCompleted Kind = "bus.close.completed"
	// KindBusCloseDrained 在消息总线关闭时排空缓冲消息后发出。
	KindBusCloseDrained Kind = "bus.close.drained"

	// KindGatewayStart 在网关启动进入运行时引导阶段时发出。
	KindGatewayStart Kind = "gateway.start"
	// KindGatewayReady 在网关服务已启动并就绪时发出。
	KindGatewayReady Kind = "gateway.ready"
	// KindGatewayShutdown 在网关开始关闭时发出。
	KindGatewayShutdown Kind = "gateway.shutdown"
	// KindGatewayReloadStarted 在网关开始重载时发出。
	KindGatewayReloadStarted Kind = "gateway.reload.started"
	// KindGatewayReloadCompleted 在网关重载完成时发出。
	KindGatewayReloadCompleted Kind = "gateway.reload.completed"
	// KindGatewayReloadFailed 在网关重载失败时发出。
	KindGatewayReloadFailed Kind = "gateway.reload.failed"

	// KindMCPServerConnected 在 MCP 服务器连接成功时发出。
	KindMCPServerConnected Kind = "mcp.server.connected"
	// KindMCPServerConnecting 在连接 MCP 服务器之前发出。
	KindMCPServerConnecting Kind = "mcp.server.connecting"
	// KindMCPServerFailed 在 MCP 服务器发生故障时发出。
	KindMCPServerFailed Kind = "mcp.server.failed"
	// KindMCPToolDiscovered 在发现 MCP 工具时发出。
	KindMCPToolDiscovered Kind = "mcp.tool.discovered"
	// KindMCPToolCallStart 在 MCP 工具调用开始时发出。
	KindMCPToolCallStart Kind = "mcp.tool.call.start"
	// KindMCPToolCallEnd 在 MCP 工具调用结束时发出。
	KindMCPToolCallEnd Kind = "mcp.tool.call.end"
)

var knownKinds = []Kind{
	KindAgentTurnStart,
	KindAgentTurnEnd,
	KindAgentLLMRequest,
	KindAgentLLMDelta,
	KindAgentLLMResponse,
	KindAgentLLMRetry,
	KindAgentContextCompress,
	KindAgentSessionSummarize,
	KindAgentToolExecStart,
	KindAgentToolExecEnd,
	KindAgentToolExecSkipped,
	KindAgentSteeringInjected,
	KindAgentAutonomyRound,
	KindAgentFollowUpQueued,
	KindAgentInterruptReceived,
	KindAgentSubTurnSpawn,
	KindAgentSubTurnEnd,
	KindAgentSubTurnResultDelivered,
	KindAgentSubTurnOrphan,
	KindAgentError,
	KindChannelLifecycleStarted,
	KindChannelLifecycleInitialized,
	KindChannelLifecycleStartFailed,
	KindChannelLifecycleStopped,
	KindChannelWebhookRegistered,
	KindChannelWebhookUnregistered,
	KindChannelMessageOutboundQueued,
	KindChannelMessageOutboundSent,
	KindChannelMessageOutboundFailed,
	KindChannelRateLimited,
	KindBusPublishFailed,
	KindBusMessageDropped,
	KindBusCloseStarted,
	KindBusCloseCompleted,
	KindBusCloseDrained,
	KindGatewayStart,
	KindGatewayReady,
	KindGatewayShutdown,
	KindGatewayReloadStarted,
	KindGatewayReloadCompleted,
	KindGatewayReloadFailed,
	KindMCPServerConnected,
	KindMCPServerConnecting,
	KindMCPServerFailed,
	KindMCPToolDiscovered,
	KindMCPToolCallStart,
	KindMCPToolCallEnd,
}

// KnownKinds 返回本包声明的运行时事件 kind。
func KnownKinds() []Kind {
	return append([]Kind(nil), knownKinds...)
}
