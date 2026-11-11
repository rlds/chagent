package providers

import (
	"math"
	"sync"
	"time"
)

const (
	defaultFailureWindow = 24 * time.Hour
)

// CooldownTracker 管理降级链中每个 provider 的冷却状态。
// 通过 sync.RWMutex 保证线程安全。仅存于内存（重启后重置）。
type CooldownTracker struct {
	mu            sync.RWMutex
	entries       map[string]*cooldownEntry
	failureWindow time.Duration
	nowFunc       func() time.Time // 用于测试
}

type cooldownEntry struct {
	ErrorCount     int
	FailureCounts  map[FailoverReason]int
	CooldownEnd    time.Time      // 标准冷却到期时间
	DisabledUntil  time.Time      // 计费相关禁用的到期时间
	DisabledReason FailoverReason // 禁用原因（计费）
	LastFailure    time.Time
}

// NewCooldownTracker 创建一个使用默认 24 小时失败窗口的追踪器。
func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{
		entries:       make(map[string]*cooldownEntry),
		failureWindow: defaultFailureWindow,
		nowFunc:       time.Now,
	}
}

// MarkFailure 记录某个 provider 的一次失败，并设置相应的冷却。
// 若上次失败距现在已经超过 failureWindow，则重置错误计数。
func (ct *CooldownTracker) MarkFailure(provider string, reason FailoverReason) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	now := ct.nowFunc()
	entry := ct.getOrCreate(provider)

	// 24 小时失败窗口重置：若 failureWindow 内没有失败，则重置计数器。
	if !entry.LastFailure.IsZero() && now.Sub(entry.LastFailure) > ct.failureWindow {
		entry.ErrorCount = 0
		entry.FailureCounts = make(map[FailoverReason]int)
	}

	entry.ErrorCount++
	entry.FailureCounts[reason]++
	entry.LastFailure = now

	if reason == FailoverBilling {
		billingCount := entry.FailureCounts[FailoverBilling]
		entry.DisabledUntil = now.Add(calculateBillingCooldown(billingCount))
		entry.DisabledReason = FailoverBilling
	} else {
		entry.CooldownEnd = now.Add(calculateStandardCooldown(entry.ErrorCount))
	}
}

// MarkSuccess 重置某个 provider 的所有计数器和冷却。
func (ct *CooldownTracker) MarkSuccess(provider string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	entry := ct.entries[provider]
	if entry == nil {
		return
	}

	entry.ErrorCount = 0
	entry.FailureCounts = make(map[FailoverReason]int)
	entry.CooldownEnd = time.Time{}
	entry.DisabledUntil = time.Time{}
	entry.DisabledReason = ""
}

// IsAvailable 在 provider 未处于冷却或禁用状态时返回 true。
func (ct *CooldownTracker) IsAvailable(provider string) bool {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	entry := ct.entries[provider]
	if entry == nil {
		return true
	}

	now := ct.nowFunc()

	// 计费禁用优先（冷却时间更长）。
	if !entry.DisabledUntil.IsZero() && now.Before(entry.DisabledUntil) {
		return false
	}

	// 标准冷却。
	if !entry.CooldownEnd.IsZero() && now.Before(entry.CooldownEnd) {
		return false
	}

	return true
}

// CooldownRemaining 返回距离 provider 恢复可用还有多久。
// 若已可用则返回 0。
func (ct *CooldownTracker) CooldownRemaining(provider string) time.Duration {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	entry := ct.entries[provider]
	if entry == nil {
		return 0
	}

	now := ct.nowFunc()
	var remaining time.Duration

	if !entry.DisabledUntil.IsZero() && now.Before(entry.DisabledUntil) {
		d := entry.DisabledUntil.Sub(now)
		if d > remaining {
			remaining = d
		}
	}

	if !entry.CooldownEnd.IsZero() && now.Before(entry.CooldownEnd) {
		d := entry.CooldownEnd.Sub(now)
		if d > remaining {
			remaining = d
		}
	}

	return remaining
}

// ErrorCount 返回某个 provider 当前的错误计数。
func (ct *CooldownTracker) ErrorCount(provider string) int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	entry := ct.entries[provider]
	if entry == nil {
		return 0
	}
	return entry.ErrorCount
}

// FailureCount 返回特定原因对应的失败计数。
func (ct *CooldownTracker) FailureCount(provider string, reason FailoverReason) int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	entry := ct.entries[provider]
	if entry == nil {
		return 0
	}
	return entry.FailureCounts[reason]
}

func (ct *CooldownTracker) getOrCreate(provider string) *cooldownEntry {
	entry := ct.entries[provider]
	if entry == nil {
		entry = &cooldownEntry{
			FailureCounts: make(map[FailoverReason]int),
		}
		ct.entries[provider] = entry
	}
	return entry
}

// calculateStandardCooldown 计算标准指数退避。
// 公式来自 OpenClaw：min(1h, 1min * 5^min(n-1, 3))
//
//	1 次错误 → 1 分钟
//	2 次错误 → 5 分钟
//	3 次错误 → 25 分钟
//	4+ 次错误 → 1 小时（上限）
func calculateStandardCooldown(errorCount int) time.Duration {
	n := max(1, errorCount)
	exp := min(n-1, 3)
	ms := 60_000 * int(math.Pow(5, float64(exp)))
	ms = min(3_600_000, ms) // 上限为 1 小时
	return time.Duration(ms) * time.Millisecond
}

// calculateBillingCooldown 计算计费相关的专属指数退避。
// 公式来自 OpenClaw：min(24h, 5h * 2^min(n-1, 10))
//
//	1 次错误 → 5 小时
//	2 次错误 → 10 小时
//	3 次错误 → 20 小时
//	4+ 次错误 → 24 小时（上限）
func calculateBillingCooldown(billingErrorCount int) time.Duration {
	const baseMs = 5 * 60 * 60 * 1000 // 5 小时
	const maxMs = 24 * 60 * 60 * 1000 // 24 小时

	n := max(1, billingErrorCount)
	exp := min(n-1, 10)
	raw := float64(baseMs) * math.Pow(2, float64(exp))
	ms := int(math.Min(float64(maxMs), raw))
	return time.Duration(ms) * time.Millisecond
}
