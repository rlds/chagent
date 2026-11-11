package routing

// Classifier 评估一组特征并返回 [0, 1] 范围内的复杂度分数。
// 分数越高，表示任务越复杂，越适合使用重量级模型。
// 该分数会与配置的阈值比较：score >= threshold 时选用
// 主（重量级）模型；score < threshold 时选用轻量模型。
//
// Classifier 定义为接口，这样未来的实现（基于机器学习、基于向量嵌入，
// 或任何其他方案）都可以直接替换进来，而无需改动路由基础设施。
type Classifier interface {
	Score(f Features) float64
}

// RuleClassifier 是 v1 实现。
// 它使用结构性信号的加权求和，不依赖任何外部组件、不发起 API 调用，
// 延迟低于一微秒。原始总和会被封顶在 1.0，以保证
// 返回的分数始终符合 [0, 1] 的约定。
//
// 各项权重（多个信号可能同时触发）：
//
//	token > 200（约 600 字符）：0.35  —— 很长的提示几乎总是复杂的
//	token 50-200：             0.15  —— 中等长度；可能复杂也可能不复杂
//	存在代码块：               0.40  —— 编码任务需要重量级模型
//	工具调用 > 3（近期）：     0.25  —— 密集的工具使用表明正在执行 agent 工作流
//	工具调用 1-3（近期）：     0.10  —— 存在一定工具活动
//	会话深度 > 10：            0.10  —— 长会话会累积隐含的复杂性
//	存在附件：                 1.00  —— 硬性门槛；多模态始终需要重量级模型
//
// 默认阈值为 0.35，因此：
//   - 纯问候 / 简单问答：                        0.00 → 轻量  ✓
//   - 中等篇幅的普通消息（50–200 tokens）：      0.15 → 轻量  ✓
//   - 带代码块的消息：                           0.40 → 重量级  ✓
//   - 长消息（>200 tokens）：                    0.35 → 重量级  ✓
//   - 活跃工具会话 + 中等篇幅消息：              0.25 → 轻量  （可接受）
//   - 任意带图片/音频附件的消息：                1.00 → 重量级  ✓
type RuleClassifier struct{}

// Score 计算给定特征集的复杂度分数。
// 返回值位于 [0, 1] 范围内。存在附件时直接短路返回 1.0。
func (c *RuleClassifier) Score(f Features) float64 {
	// 硬性门槛：多模态输入始终需要重量级模型。
	if f.HasAttachments {
		return 1.0
	}

	var score float64

	// Token 估算 —— 主要的篇幅信号
	switch {
	case f.TokenEstimate > 200:
		score += 0.35
	case f.TokenEstimate > 50:
		score += 0.15
	}

	// 围栏代码块 —— 编码/技术类任务最有力的指示信号
	if f.CodeBlockCount > 0 {
		score += 0.40
	}

	// 近期工具调用密度 —— 表明正在进行中的 agent 工作流
	switch {
	case f.RecentToolCalls > 3:
		score += 0.25
	case f.RecentToolCalls > 0:
		score += 0.10
	}

	// 会话深度 —— 累积的上下文意味着复合型任务
	if f.ConversationDepth > 10 {
		score += 0.10
	}

	// 封顶为 1.0，以便在多个信号同时触发时仍然遵守 [0, 1] 约定
	//（例如长消息 + 代码块 + 工具链 = 原始值 1.10）。
	if score > 1.0 {
		score = 1.0
	}
	return score
}
