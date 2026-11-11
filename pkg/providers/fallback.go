package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/logger"
)

// FallbackChain 协调多个候选之间的模型降级。
type FallbackChain struct {
	cooldown *CooldownTracker
	rl       *RateLimiterRegistry
}

// FallbackCandidate 表示一个可供尝试的模型/provider。
type FallbackCandidate struct {
	Provider    string
	Model       string
	DisplayName string // 用于持久化/UI 的可选配置别名或原始模型标签
	RPM         int    // 每分钟请求数；0 表示不限制
	IdentityKey string // 用于冷却/限流的可选稳定配置标识
}

// StableKey 在可用时返回候选对象的配置级标识，
// 否则回退到运行时的 provider/model 键。
func (c FallbackCandidate) StableKey() string {
	if key := strings.TrimSpace(c.IdentityKey); key != "" {
		return key
	}
	return ModelKey(c.Provider, c.Model)
}

// FallbackResult 包含成功的响应以及所有尝试的元数据。
type FallbackResult struct {
	Response    *LLMResponse
	Provider    string
	Model       string
	IdentityKey string
	Attempts    []FallbackAttempt
}

// FallbackAttempt 记录降级链中的一次尝试。
type FallbackAttempt struct {
	Provider string
	Model    string
	Error    error
	Reason   FailoverReason
	Duration time.Duration
	Skipped  bool // 若因冷却被跳过则为 true
}

// NewFallbackChain 使用给定的冷却追踪器和限流器注册表创建一个新的降级链。
func NewFallbackChain(cooldown *CooldownTracker, rl *RateLimiterRegistry) *FallbackChain {
	return &FallbackChain{cooldown: cooldown, rl: rl}
}

// ResolveCandidates 将模型配置解析为去重后的候选列表。
func ResolveCandidates(cfg ModelConfig, defaultProvider string) []FallbackCandidate {
	return ResolveCandidatesWithLookup(cfg, defaultProvider, nil)
}

func ResolveCandidatesWithLookup(
	cfg ModelConfig,
	defaultProvider string,
	lookup func(raw string) (resolved string, ok bool),
) []FallbackCandidate {
	seen := make(map[string]bool)
	var candidates []FallbackCandidate

	addCandidate := func(raw string) {
		candidateRaw := strings.TrimSpace(raw)
		if lookup != nil {
			if resolved, ok := lookup(candidateRaw); ok {
				candidateRaw = resolved
			}
		}

		ref := ParseModelRef(candidateRaw, defaultProvider)
		if ref == nil {
			return
		}
		key := ModelKey(ref.Provider, ref.Model)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, FallbackCandidate{
			Provider:    ref.Provider,
			Model:       ref.Model,
			DisplayName: candidateRaw,
		})
	}

	// 首选候选。
	addCandidate(cfg.Primary)

	// 然后是降级候选。
	for _, fb := range cfg.Fallbacks {
		addCandidate(fb)
	}

	return candidates
}

// Execute 为文本/聊天请求运行降级链。
// 它按顺序尝试每个候选，并遵守冷却与错误分类。
//
// 行为：
//   - 处于冷却中的候选会被跳过（记录为跳过尝试）。
//   - context.Canceled 立即中止（用户中止，不降级）。
//   - 不可重试的错误（格式）立即中止。
//   - 可重试的错误触发降级到下一个候选。
//   - 成功会将 provider 标记为正常（重置冷却）。
//   - 若全部失败，返回包含所有尝试的聚合错误。
func (fc *FallbackChain) Execute(
	ctx context.Context,
	candidates []FallbackCandidate,
	run func(ctx context.Context, provider, model string) (*LLMResponse, error),
) (*FallbackResult, error) {
	return fc.ExecuteCandidate(
		ctx,
		candidates,
		func(ctx context.Context, candidate FallbackCandidate) (*LLMResponse, error) {
			return run(ctx, candidate.Provider, candidate.Model)
		},
	)
}

// ExecuteCandidate 运行降级链，并将完整的候选对象传递给调用方，
// 以便保留模型列表身份元数据。
func (fc *FallbackChain) ExecuteCandidate(
	ctx context.Context,
	candidates []FallbackCandidate,
	run func(ctx context.Context, candidate FallbackCandidate) (*LLMResponse, error),
) (*FallbackResult, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("fallback: no candidates configured")
	}

	result := &FallbackResult{
		Attempts: make([]FallbackAttempt, 0, len(candidates)),
	}

	for i, candidate := range candidates {
		// 每次尝试前检查上下文。
		if ctx.Err() == context.Canceled {
			return nil, context.Canceled
		}

		// 按稳定的候选标识检查冷却，而不仅仅是 provider/model。
		// 这样别名和多 key 配置可以各自独立地故障转移。
		cooldownKey := candidate.StableKey()
		if !fc.cooldown.IsAvailable(cooldownKey) {
			remaining := fc.cooldown.CooldownRemaining(cooldownKey)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Skipped:  true,
				Reason:   FailoverRateLimit,
				Error: fmt.Errorf(
					"%s in cooldown (%s remaining)",
					cooldownKey,
					remaining.Round(time.Second),
				),
			})
			continue
		}

		// 调用 provider 之前，先执行按候选计算的限流。
		// 若该候选在本地已达到饱和，则先尝试其他候选。
		if fc.rl != nil {
			if !fc.rl.TryAcquire(cooldownKey) {
				if i < len(candidates)-1 {
					result.Attempts = append(result.Attempts, FallbackAttempt{
						Provider: candidate.Provider,
						Model:    candidate.Model,
						Skipped:  true,
						Reason:   FailoverRateLimit,
						Error:    fmt.Errorf("%s waiting for local rate limit token", cooldownKey),
					})
					continue
				}
				if waitErr := fc.rl.Wait(ctx, cooldownKey); waitErr != nil {
					result.Attempts = append(result.Attempts, FallbackAttempt{
						Provider: candidate.Provider,
						Model:    candidate.Model,
						Skipped:  true,
						Reason:   FailoverRateLimit,
						Error:    waitErr,
					})
					return nil, waitErr
				}
			}
		}

		// 执行 run 函数。
		start := time.Now()
		resp, err := run(ctx, candidate)
		elapsed := time.Since(start)

		if err == nil {
			// 成功。以 Info 级别记录降级恢复，这样运维人员无需开启
			// debug 日志也能确认故障转移（robustness A8）。
			fc.cooldown.MarkSuccess(cooldownKey)
			if i > 0 || len(result.Attempts) > 0 {
				logger.InfoCF("providers", "Fallback: succeeded",
					map[string]any{
						"provider": candidate.Provider,
						"model":    candidate.Model,
						"attempts": len(result.Attempts) + 1,
					})
			}
			result.Response = resp
			result.Provider = candidate.Provider
			result.Model = candidate.Model
			result.IdentityKey = candidate.StableKey()
			return result, nil
		}

		// 上下文被取消：立即中止，不降级。
		if ctx.Err() == context.Canceled {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Duration: elapsed,
			})
			return nil, context.Canceled
		}

		// 对错误进行分类。
		failErr := ClassifyError(err, candidate.Provider, candidate.Model)

		if failErr == nil {
			// 无法归类的错误：不降级，立即返回。
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Duration: elapsed,
			})
			return nil, fmt.Errorf("fallback: unclassified error from %s/%s: %w",
				candidate.Provider, candidate.Model, err)
		}

		// 不可重试的错误：立即中止。
		if !failErr.IsRetriable() {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    failErr,
				Reason:   failErr.Reason,
				Duration: elapsed,
			})
			return nil, failErr
		}

		// 可重试的错误：标记失败并继续下一个候选。
		fc.cooldown.MarkFailure(cooldownKey, failErr.Reason)
		result.Attempts = append(result.Attempts, FallbackAttempt{
			Provider: candidate.Provider,
			Model:    candidate.Model,
			Error:    failErr,
			Reason:   failErr.Reason,
			Duration: elapsed,
		})

		// 如果这是最后一个候选，则返回聚合错误。
		if i == len(candidates)-1 {
			return nil, &FallbackExhaustedError{Attempts: result.Attempts}
		}
	}

	// 所有候选都被跳过（全部处于冷却中）。
	return nil, &FallbackExhaustedError{Attempts: result.Attempts}
}

// ExecuteImage 为图像/视觉请求运行降级链。
// 比 Execute 更简单：不做冷却检查（图像端点的限流规则不同）。
// 图像尺寸/大小错误会立即中止（不可重试）。
func (fc *FallbackChain) ExecuteImage(
	ctx context.Context,
	candidates []FallbackCandidate,
	run func(ctx context.Context, provider, model string) (*LLMResponse, error),
) (*FallbackResult, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("image fallback: no candidates configured")
	}

	result := &FallbackResult{
		Attempts: make([]FallbackAttempt, 0, len(candidates)),
	}

	for i, candidate := range candidates {
		if ctx.Err() == context.Canceled {
			return nil, context.Canceled
		}

		// 调用 provider 之前，先执行按候选计算的限流。
		// 若该候选在本地已达到饱和，则先尝试其他候选。
		imageKey := candidate.StableKey()
		if fc.rl != nil {
			if !fc.rl.TryAcquire(imageKey) {
				if i < len(candidates)-1 {
					result.Attempts = append(result.Attempts, FallbackAttempt{
						Provider: candidate.Provider,
						Model:    candidate.Model,
						Skipped:  true,
						Reason:   FailoverRateLimit,
						Error:    fmt.Errorf("%s waiting for local rate limit token", imageKey),
					})
					continue
				}
				if waitErr := fc.rl.Wait(ctx, imageKey); waitErr != nil {
					result.Attempts = append(result.Attempts, FallbackAttempt{
						Provider: candidate.Provider,
						Model:    candidate.Model,
						Skipped:  true,
						Reason:   FailoverRateLimit,
						Error:    waitErr,
					})
					return nil, waitErr
				}
			}
		}

		start := time.Now()
		resp, err := run(ctx, candidate.Provider, candidate.Model)
		elapsed := time.Since(start)

		if err == nil {
			result.Response = resp
			result.Provider = candidate.Provider
			result.Model = candidate.Model
			result.IdentityKey = candidate.StableKey()
			return result, nil
		}

		if ctx.Err() == context.Canceled {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Duration: elapsed,
			})
			return nil, context.Canceled
		}

		// 图像尺寸/大小错误不可重试。
		errMsg := strings.ToLower(err.Error())
		if IsImageDimensionError(errMsg) || IsImageSizeError(errMsg) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Reason:   FailoverFormat,
				Duration: elapsed,
			})
			return nil, &FailoverError{
				Reason:   FailoverFormat,
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Wrapped:  err,
			}
		}

		// 其他任何错误：记录下来并尝试下一个。
		result.Attempts = append(result.Attempts, FallbackAttempt{
			Provider: candidate.Provider,
			Model:    candidate.Model,
			Error:    err,
			Duration: elapsed,
		})

		if i == len(candidates)-1 {
			return nil, &FallbackExhaustedError{Attempts: result.Attempts}
		}
	}

	return nil, &FallbackExhaustedError{Attempts: result.Attempts}
}

// FallbackExhaustedError 表示所有降级候选都已尝试且均失败。
type FallbackExhaustedError struct {
	Attempts []FallbackAttempt
}

func (e *FallbackExhaustedError) Error() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("fallback: all %d candidates failed:", len(e.Attempts)))
	for i, a := range e.Attempts {
		if a.Skipped {
			sb.WriteString(fmt.Sprintf("\n  [%d] %s/%s: skipped (cooldown)", i+1, a.Provider, a.Model))
		} else {
			sb.WriteString(fmt.Sprintf("\n  [%d] %s/%s: %v (reason=%s, %s)",
				i+1, a.Provider, a.Model, a.Error, a.Reason, a.Duration.Round(time.Millisecond)))
		}
	}
	return sb.String()
}
