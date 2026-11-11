package routing

import (
	"github.com/rlds/chagent/pkg/providers"
)

// defaultThreshold 在配置的阈值为零或负数时使用。
// 取 0.35 时，消息至少需要具备一个强信号（代码块、长文本
// 或附件）才会选择重量级模型。
const defaultThreshold = 0.35

// RouterConfig 保存经过校验的模型路由设置。
// 它的字段与 config.RoutingConfig 对应，但定义在 pkg/routing 中，
// 以保持依赖关系简单：由 pkg/agent 解析 config → routing，而非反向依赖。
type RouterConfig struct {
	// LightModel 是用于简单任务的 model_name（来自 model_list）。
	LightModel string

	// Threshold 是 [0, 1] 范围内的复杂度分数阈值。
	// score >= Threshold → 主（重量级）模型。
	// score <  Threshold → 轻量模型。
	Threshold float64
}

// Router 为每条入站消息选择合适的模型层级。
// 它可安全地被多个 goroutine 并发使用。
type Router struct {
	cfg        RouterConfig
	classifier Classifier
}

// New 使用给定配置和默认的 RuleClassifier 创建一个 Router。
// 如果 cfg.Threshold 为零或负数，则使用 defaultThreshold（0.35）。
func New(cfg RouterConfig) *Router {
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultThreshold
	}
	return &Router{
		cfg:        cfg,
		classifier: &RuleClassifier{},
	}
}

// newWithClassifier 使用自定义 Classifier 创建一个 Router。
// 供需要注入确定性评分器的单元测试使用。
func newWithClassifier(cfg RouterConfig, c Classifier) *Router {
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultThreshold
	}
	return &Router{cfg: cfg, classifier: c}
}

// SelectModel 返回本轮对话应使用的模型，
// 以及计算出的复杂度分数（用于日志记录和调试）。
//
//   - 如果 score < cfg.Threshold：返回 (cfg.LightModel, true, score)
//   - 否则：                     返回 (primaryModel, false, score)
//
// 调用方负责将返回的模型名解析为
// 提供方候选（参见 AgentInstance.LightCandidates）。
func (r *Router) SelectModel(
	msg string,
	history []providers.Message,
	primaryModel string,
) (model string, usedLight bool, score float64) {
	features := ExtractFeatures(msg, history)
	score = r.classifier.Score(features)
	if score < r.cfg.Threshold {
		return r.cfg.LightModel, true, score
	}
	return primaryModel, false, score
}

// LightModel 返回配置的轻量模型名称。
func (r *Router) LightModel() string {
	return r.cfg.LightModel
}

// Threshold 返回当前使用的复杂度阈值。
func (r *Router) Threshold() float64 {
	return r.cfg.Threshold
}
