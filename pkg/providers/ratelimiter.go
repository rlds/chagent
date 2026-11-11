package providers

import (
	"context"
	"sync"
	"time"
)

// RateLimiter 针对单个键实现令牌桶限流。
// 每分钟最多允许 RPM 个请求，突发容量等于 RPM。
// 线程安全。
type RateLimiter struct {
	mu       sync.Mutex
	rpm      int
	tokens   float64
	maxBurst float64
	lastTick time.Time
	nowFunc  func() time.Time // 用于测试
}

func (rl *RateLimiter) refillLocked(now time.Time) {
	elapsed := now.Sub(rl.lastTick).Seconds()
	rl.lastTick = now

	// 按经过的时间成比例补充令牌。
	refill := elapsed * float64(rl.rpm) / 60.0
	rl.tokens = min(rl.maxBurst, rl.tokens+refill)
}

// newRateLimiter 创建一个每分钟允许 rpm 个请求的 RateLimiter。
func newRateLimiter(rpm int) *RateLimiter {
	return &RateLimiter{
		rpm:      rpm,
		tokens:   float64(rpm), // 初始为满
		maxBurst: float64(rpm),
		lastTick: time.Now(),
		nowFunc:  time.Now,
	}
}

// Wait 会阻塞直到有可用令牌，或 ctx 被取消。
// 若在等待期间被取消，则返回 ctx.Err()。
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		now := rl.nowFunc()
		rl.refillLocked(now)

		if rl.tokens >= 1.0 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}

		// 计算距离下一个令牌可用还需多久。
		deficit := 1.0 - rl.tokens
		waitSec := deficit / (float64(rl.rpm) / 60.0)
		rl.mu.Unlock()

		timer := time.NewTimer(time.Duration(waitSec * float64(time.Second)))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
			// 循环重新检查（另一个 goroutine 可能已消耗掉该令牌）。
		}
	}
}

// TryAcquire 尝试在不阻塞的情况下消耗一个令牌。
func (rl *RateLimiter) TryAcquire() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.refillLocked(rl.nowFunc())
	if rl.tokens < 1.0 {
		return false
	}
	rl.tokens--
	return true
}

// RateLimiterRegistry 保存各候选各自的限流器。
// RPM=0 的候选不受限制。
// 并发读写安全。
type RateLimiterRegistry struct {
	mu       sync.RWMutex
	limiters map[string]*RateLimiter
}

// NewRateLimiterRegistry 创建一个空注册表。
func NewRateLimiterRegistry() *RateLimiterRegistry {
	return &RateLimiterRegistry{
		limiters: make(map[string]*RateLimiter),
	}
}

// Register 以给定的 RPM 为指定键添加一个限流器。
// 若 rpm <= 0，则不注册限流器（不受限制）。
func (r *RateLimiterRegistry) Register(key string, rpm int) {
	if rpm <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limiters[key] = newRateLimiter(rpm)
}

// Wait 为指定键获取一个令牌，必要时会阻塞。
// 若该键未注册限流器，则立即返回。
func (r *RateLimiterRegistry) Wait(ctx context.Context, key string) error {
	r.mu.RLock()
	rl := r.limiters[key]
	r.mu.RUnlock()
	if rl == nil {
		return nil
	}
	return rl.Wait(ctx)
}

// TryAcquire 尝试在不阻塞的情况下为指定键消耗一个令牌。
// 若该键未注册限流器，则返回 true。
func (r *RateLimiterRegistry) TryAcquire(key string) bool {
	r.mu.RLock()
	rl := r.limiters[key]
	r.mu.RUnlock()
	if rl == nil {
		return true
	}
	return rl.TryAcquire()
}

// RegisterCandidates 为所有 RPM > 0 的候选注册限流器。
// RPM == 0 的候选会被忽略（不受限制）。
func (r *RateLimiterRegistry) RegisterCandidates(candidates []FallbackCandidate) {
	for _, c := range candidates {
		if c.RPM > 0 {
			r.Register(c.StableKey(), c.RPM)
		}
	}
}
