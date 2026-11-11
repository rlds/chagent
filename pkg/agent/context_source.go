// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// 上下文来源注册（T4.1）。
//
// PromptContributor 产出 system prompt 片段（稳定、可前缀缓存的能力/指令文本）。
// ContextSource 则产出一致的上下文消息（运行时快照、任务状态、技能目录、工作区指令增量）：
// 这些值会在轮次中变化，必须可回放并能在会话历史中对账。
//
// 选择规则：
//   - 与值无关、长期存在、必须出现在缓存前缀中 -> PromptContributor / system 片段；
//   - 依赖值或文件状态、只在变化时注入、之后可能被替换或移除 -> ContextSource / 持久上下文消息。
//
// 未注册来源时行为不变：没有任何上下文来源时，CollectContext 返回 nil，请求逐字节不变。

// 上下文来源的顺序边界。Order 超出范围的贡献会被钳制并记录日志；
// 这能防止行为异常的插件插到核心上下文之前（workspace < runtime < skill catalog < task state < loop guard）。
const (
	ContextSourceOrderMin = -1000
	ContextSourceOrderMax = 1000

	// 来源未设置 Order 时使用 ContextContributionDefaultOrder。
	ContextContributionDefaultOrder = 0
)

// ContextContribution 是 ContextSource 提出的一条持久上下文消息。
// Text 是模型可见内容（约定使用中文）；Meta 携带类型化来源信息，用于历史对账。
type ContextContribution struct {
	Text string
	Meta protocoltypes.ContextMeta

	// Required 标记必须经受可选上下文抑制的操作性事实（例如任务状态或技能目录）。
	// 即使 PromptBuildRequest 要求丢弃可选上下文，Required 贡献也会输出，
	// 并且优先于之后具有相同 Meta.Kind+scope 的可选贡献。
	Required bool

	// Order 按升序排序贡献；顺序相同则回退到来源 ID，再回退到来源的返回顺序。
	Order int
}

// ContextSource 产出持久上下文消息而不是 system prompt 片段。
// 实现必须能安全地并发调用 CollectContext。
type ContextSource interface {
	ContextSource() PromptSourceDescriptor
	ContributeContext(ctx context.Context, req PromptBuildRequest) ([]ContextContribution, error)
}

// RegisterContextSource 注册一个上下文来源。描述符必须带 ID 和 Owner；
// 重复 ID 会被拒绝（静默替换来源会让注入顺序与来源信息变得含糊）。
func (r *PromptRegistry) RegisterContextSource(source ContextSource) error {
	if r == nil {
		return fmt.Errorf("prompt registry is nil")
	}
	if source == nil {
		return fmt.Errorf("context source is nil")
	}
	desc, err := contextSourceDescriptorSafely(source)
	if err != nil {
		return err
	}
	sourceID := PromptSourceID(strings.TrimSpace(string(desc.ID)))
	if sourceID == "" {
		return fmt.Errorf("context source id is required")
	}
	if strings.TrimSpace(desc.Owner) == "" {
		return fmt.Errorf("context source %q must declare an owner", sourceID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.contextSourceIDs == nil {
		r.contextSourceIDs = make(map[PromptSourceID]struct{})
	}
	if _, exists := r.contextSourceIDs[sourceID]; exists {
		return fmt.Errorf("context source %q is already registered", sourceID)
	}
	r.contextSourceIDs[sourceID] = struct{}{}
	r.contextSources = append(r.contextSources, source)
	return nil
}

// contextSourceDescriptorSafely 读取来源描述符，避免插件 panic 拖垮注册流程。
func contextSourceDescriptorSafely(source ContextSource) (desc PromptSourceDescriptor, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			desc = PromptSourceDescriptor{}
			err = fmt.Errorf("context source descriptor panicked: %v", recovered)
		}
	}()
	return source.ContextSource(), nil
}

// CollectContext 运行所有已注册的上下文来源，返回下一次 LLM 调用前要注入的持久上下文消息。
//
// 行为：
//   - 来源按注册顺序运行；贡献先按 Order 升序排序，再按来源 ID，再按来源返回顺序；
//   - 空贡献/仅含空白的贡献被丢弃；
//   - req.SuppressOptionalContext 为真时丢弃可选贡献，Required 贡献保留；
//   - 相同 Meta.Kind 和 scope 的贡献合并为一条消息（后者胜），但 Required 贡献绝不会被
//     之后的可选贡献替换；scope 使用 Meta.BaselineIdentity，因此按文件的工作区上下文不会跨文件合并；
//   - 失败（或 panic）的来源以 warn 级别记录并跳过；绝不让回合失败，也不抑制其他来源；
//   - 没有已注册来源时结果为 nil（保持旧行为）。
func (r *PromptRegistry) CollectContext(ctx context.Context, req PromptBuildRequest) []providers.Message {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	sources := append([]ContextSource(nil), r.contextSources...)
	r.mu.RUnlock()
	if len(sources) == 0 {
		return nil
	}

	type orderedContribution struct {
		sourceID     PromptSourceID
		contribution ContextContribution
		seq          int
	}

	contributions := make([]orderedContribution, 0, len(sources))
	seq := 0
	for _, source := range sources {
		if source == nil {
			continue
		}
		desc, contributed, err := collectContextSourceSafely(ctx, source, req)
		sourceID := PromptSourceID(strings.TrimSpace(string(desc.ID)))
		if err != nil {
			logger.WarnCF("agent", "Context source contribution failed; skipping source", map[string]any{
				"source": sourceID,
				"error":  err.Error(),
			})
			continue
		}
		for _, contribution := range contributed {
			if req.SuppressOptionalContext && !contribution.Required {
				continue
			}
			if strings.TrimSpace(contribution.Text) == "" {
				continue
			}
			order, clamped := normalizeContextSourceOrder(contribution.Order)
			if clamped {
				logger.WarnCF("agent", "Context contribution order out of range; clamped", map[string]any{
					"source":     sourceID,
					"order":      contribution.Order,
					"clamped_to": order,
				})
			}
			contribution.Order = order
			contributions = append(contributions, orderedContribution{
				sourceID:     sourceID,
				contribution: contribution,
				seq:          seq,
			})
			seq++
		}
	}
	if len(contributions) == 0 {
		return nil
	}

	slices.SortStableFunc(contributions, func(a, b orderedContribution) int {
		if a.contribution.Order != b.contribution.Order {
			return a.contribution.Order - b.contribution.Order
		}
		if a.sourceID != b.sourceID {
			return strings.Compare(string(a.sourceID), string(b.sourceID))
		}
		return a.seq - b.seq
	})

	type dedupeKey struct {
		kind  string
		scope string
	}

	messages := make([]providers.Message, 0, len(contributions))
	required := make([]bool, 0, len(contributions))
	index := make(map[dedupeKey]int, len(contributions))
	for _, ordered := range contributions {
		meta := cloneContextMeta(ordered.contribution.Meta)
		meta.Kind = strings.TrimSpace(meta.Kind)
		if meta.Kind == "" {
			// 未命名贡献仍需要稳定的来源信息用于历史对账和去重；回退到来源 ID。
			meta.Kind = string(ordered.sourceID)
		}
		key := dedupeKey{kind: meta.Kind, scope: contextMetaScope(meta)}
		if existing, ok := index[key]; ok {
			if required[existing] && !ordered.contribution.Required {
				// Required 上下文是权威的；同一 kind+scope 之后的可选消息会被跳过。
				continue
			}
			messages[existing] = contextContributionMessage(ordered.sourceID, ordered.contribution, meta)
			required[existing] = ordered.contribution.Required
			continue
		}
		index[key] = len(messages)
		messages = append(messages, contextContributionMessage(ordered.sourceID, ordered.contribution, meta))
		required = append(required, ordered.contribution.Required)
	}

	return messages
}

// collectContextSourceSafely 隔离单个来源：返回错误或 panic
// （来自描述符或 ContributeContext）由调用方转换为警告，而不是让回合失败。
func collectContextSourceSafely(
	ctx context.Context,
	source ContextSource,
	req PromptBuildRequest,
) (desc PromptSourceDescriptor, contributions []ContextContribution, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			contributions = nil
			err = fmt.Errorf("context source panicked: %v", recovered)
		}
	}()
	desc = source.ContextSource()
	contributions, err = source.ContributeContext(ctx, req)
	return desc, contributions, err
}

func normalizeContextSourceOrder(order int) (int, bool) {
	switch {
	case order < ContextSourceOrderMin:
		return ContextSourceOrderMin, true
	case order > ContextSourceOrderMax:
		return ContextSourceOrderMax, true
	default:
		return order, false
	}
}

// contextMetaScope 推导上下文消息的去重 scope。
// ContextMeta 没有专门的 Scope 字段，因此用 BaselineIdentity（基线渲染来源的
// 工作区身份）充当该角色。没有基线身份的贡献共享空 scope：每种 Kind 最多保留一条。
func contextMetaScope(meta protocoltypes.ContextMeta) string {
	return strings.TrimSpace(meta.BaselineIdentity)
}

func cloneContextMeta(meta protocoltypes.ContextMeta) protocoltypes.ContextMeta {
	cloned := meta
	cloned.Changes = append([]protocoltypes.ContextChange(nil), meta.Changes...)
	cloned.Sections = append([]protocoltypes.ContextSection(nil), meta.Sections...)
	return cloned
}

func contextContributionMessage(
	sourceID PromptSourceID,
	contribution ContextContribution,
	meta protocoltypes.ContextMeta,
) providers.Message {
	return providers.Message{
		Role:         "user",
		Content:      contribution.Text,
		Context:      &meta,
		PromptLayer:  string(PromptLayerContext),
		PromptSlot:   string(PromptSlotContext),
		PromptSource: string(sourceID),
	}
}

// RegisterContextSource 在 builder 的 prompt registry 上注册上下文来源
// （必要时创建 registry）。
func (cb *ContextBuilder) RegisterContextSource(source ContextSource) error {
	if cb == nil {
		return fmt.Errorf("context builder is nil")
	}
	return cb.promptRegistryOrDefault().RegisterContextSource(source)
}

// WithContextSources 注册零个或多个插件/hook 上下文来源。
// 注册失败会被记录并跳过；绝不会 panic。
func (cb *ContextBuilder) WithContextSources(sources ...ContextSource) *ContextBuilder {
	if cb == nil {
		return cb
	}
	for _, source := range sources {
		if err := cb.RegisterContextSource(source); err != nil {
			logger.WarnCF("agent", "Failed to register context source", map[string]any{
				"error": err.Error(),
			})
		}
	}
	return cb
}

// HasContextSources 报告是否注册了任何上下文来源。
// pre-step 注入器用它保证无来源快路径与旧行为逐字节一致。
func (cb *ContextBuilder) HasContextSources() bool {
	if cb == nil {
		return false
	}
	return cb.promptRegistryOrDefault().HasContextSources()
}

// HasContextSources 报告 registry 是否至少有一个上下文来源。
func (r *PromptRegistry) HasContextSources() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.contextSources) > 0
}

// CollectContextMessages 返回注册的上下文来源为本次请求贡献的持久上下文消息。
// pre-step 注入表每次 LLM 调用调用一次；没有注册来源时返回 nil，因此未修改的调用方保持原有行为。
func (cb *ContextBuilder) CollectContextMessages(ctx context.Context, req PromptBuildRequest) []providers.Message {
	if cb == nil {
		return nil
	}
	return cb.promptRegistryOrDefault().CollectContext(ctx, req)
}
