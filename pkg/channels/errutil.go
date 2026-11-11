package channels

import (
	"fmt"
	"net/http"
)

// ClassifySendError 根据 HTTP 状态码，用相应的哨兵错误包装原始错误。
// 需要调用 HTTP API 的 channel 应在它们的 Send 路径中使用它。
func ClassifySendError(statusCode int, rawErr error) error {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", ErrRateLimit, rawErr)
	case statusCode >= 500:
		return fmt.Errorf("%w: %w", ErrTemporary, rawErr)
	case statusCode >= 400:
		return fmt.Errorf("%w: %w", ErrSendFailed, rawErr)
	default:
		return rawErr
	}
}

// ClassifyNetError 将网络/超时错误包装为 ErrTemporary。
func ClassifyNetError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTemporary, err)
}
