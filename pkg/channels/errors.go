package channels

import "errors"

var (
	// ErrNotRunning 表示该 channel 未在运行中。
	// Manager 不会重试。
	ErrNotRunning = errors.New("channel not running")

	// ErrRateLimit 表示平台返回了限流响应（例如 HTTP 429）。
	// Manager 会等待固定延迟后重试。
	ErrRateLimit = errors.New("rate limited")

	// ErrTemporary 表示瞬时故障（例如网络超时、5xx）。
	// Manager 会使用指数退避后重试。
	ErrTemporary = errors.New("temporary failure")

	// ErrSendFailed 表示永久性失败（例如无效的 chat ID、非 429 的 4xx）。
	// Manager 不会重试。
	ErrSendFailed = errors.New("send failed")
)
